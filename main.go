package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/generative-ai-go/genai"
	"github.com/gorilla/websocket"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/api/option"
)

// ── Config ──────────────────────────────────────────────────────────────────

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	JWTSecret    = []byte(getenv("JWT_SECRET", "blazvex-super-secret-change-me"))
	GeminiAPIKey = getenv("GEMINI_API_KEY", "")
	Port         = getenv("PORT", "8080")
	UploadDir    = getenv("UPLOAD_DIR", "./uploads")
	DBPath       = getenv("DB_PATH", "./blazvex.db")
	MaxUpload    = int64(50 << 20)
)

// ── Database ─────────────────────────────────────────────────────────────────

var db *sql.DB

func initDB() {
	var err error
	db, err = sql.Open("sqlite3", DBPath+"?_journal=WAL&_timeout=5000")
	if err != nil {
		log.Fatal("DB open:", err)
	}
	db.SetMaxOpenConns(1)

	schema := `
	PRAGMA foreign_keys = ON;

	CREATE TABLE IF NOT EXISTS users (
		id         TEXT PRIMARY KEY,
		username   TEXT UNIQUE NOT NULL,
		email      TEXT UNIQUE NOT NULL,
		password   TEXT NOT NULL,
		name       TEXT NOT NULL,
		role       TEXT DEFAULT '',
		bio        TEXT DEFAULT '',
		avatar_url TEXT DEFAULT '',
		online     INTEGER DEFAULT 0,
		created_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS messages (
		id         TEXT PRIMARY KEY,
		from_id    TEXT NOT NULL,
		to_id      TEXT NOT NULL,
		text       TEXT DEFAULT '',
		code       TEXT DEFAULT '',
		lang       TEXT DEFAULT '',
		file_url   TEXT DEFAULT '',
		file_name  TEXT DEFAULT '',
		type       TEXT NOT NULL DEFAULT 'text',
		created_at TEXT NOT NULL,
		FOREIGN KEY(from_id) REFERENCES users(id),
		FOREIGN KEY(to_id)   REFERENCES users(id)
	);

	CREATE TABLE IF NOT EXISTS posts (
		id         TEXT PRIMARY KEY,
		user_id    TEXT NOT NULL,
		text       TEXT NOT NULL,
		code       TEXT DEFAULT '',
		lang       TEXT DEFAULT '',
		likes      INTEGER DEFAULT 0,
		comments   INTEGER DEFAULT 0,
		created_at TEXT NOT NULL,
		FOREIGN KEY(user_id) REFERENCES users(id)
	);

	CREATE TABLE IF NOT EXISTS follows (
		follower_id TEXT NOT NULL,
		followee_id TEXT NOT NULL,
		PRIMARY KEY(follower_id, followee_id)
	);

	CREATE TABLE IF NOT EXISTS post_likes (
		user_id TEXT NOT NULL,
		post_id TEXT NOT NULL,
		PRIMARY KEY(user_id, post_id)
	);
	`
	if _, err := db.Exec(schema); err != nil {
		log.Fatal("Schema:", err)
	}
	log.Println("✅ Database initialized")
}

func genID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ── Models ───────────────────────────────────────────────────────────────────

type User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Email     string `json:"email,omitempty"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Bio       string `json:"bio"`
	AvatarURL string `json:"avatar_url"`
	Online    bool   `json:"online"`
	CreatedAt string `json:"created_at"`
	Following bool   `json:"following,omitempty"`
	Followers int    `json:"followers,omitempty"`
}

type Message struct {
	ID        string `json:"id"`
	FromID    string `json:"from_id"`
	ToID      string `json:"to_id"`
	Text      string `json:"text,omitempty"`
	Code      string `json:"code,omitempty"`
	Lang      string `json:"lang,omitempty"`
	FileURL   string `json:"file_url,omitempty"`
	FileName  string `json:"file_name,omitempty"`
	Type      string `json:"type"`
	CreatedAt string `json:"created_at"`
}

type Post struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	User      *User  `json:"user,omitempty"`
	Text      string `json:"text"`
	Code      string `json:"code,omitempty"`
	Lang      string `json:"lang,omitempty"`
	Likes     int    `json:"likes"`
	Comments  int    `json:"comments"`
	Liked     bool   `json:"liked,omitempty"`
	CreatedAt string `json:"created_at"`
}

type Claims struct {
	UserID string `json:"user_id"`
	jwt.RegisteredClaims
}

// ── JWT ──────────────────────────────────────────────────────────────────────

func signToken(userID string) (string, error) {
	c := Claims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(30 * 24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(JWTSecret)
}

func parseToken(tokenStr string) (*Claims, error) {
	t, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		return JWTSecret, nil
	})
	if err != nil || !t.Valid {
		return nil, fmt.Errorf("invalid token")
	}
	return t.Claims.(*Claims), nil
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			fail(w, 401, "missing token")
			return
		}
		claims, err := parseToken(strings.TrimPrefix(auth, "Bearer "))
		if err != nil {
			fail(w, 401, "invalid token")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxKey("uid"), claims.UserID))
		next(w, r)
	}
}

type ctxKey string

func uid(r *http.Request) string {
	v, _ := r.Context().Value(ctxKey("uid")).(string)
	return v
}

// ── WebSocket Hub ─────────────────────────────────────────────────────────────

type WSClient struct {
	ID   string
	Conn *websocket.Conn
	Send chan []byte
}

type Hub struct {
	mu      sync.RWMutex
	clients map[string]*WSClient
}

var hub = &Hub{clients: make(map[string]*WSClient)}

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

func (h *Hub) Register(c *WSClient) {
	h.mu.Lock()
	h.clients[c.ID] = c
	h.mu.Unlock()
	db.Exec("UPDATE users SET online=1 WHERE id=?", c.ID)
	h.Broadcast(mustMarshal(map[string]interface{}{"type": "presence", "payload": map[string]interface{}{"user_id": c.ID, "online": true}}))
}

func (h *Hub) Unregister(id string) {
	h.mu.Lock()
	if c, ok := h.clients[id]; ok {
		close(c.Send)
		delete(h.clients, id)
	}
	h.mu.Unlock()
	db.Exec("UPDATE users SET online=0 WHERE id=?", id)
	h.Broadcast(mustMarshal(map[string]interface{}{"type": "presence", "payload": map[string]interface{}{"user_id": id, "online": false}}))
}

func (h *Hub) SendTo(toID string, data []byte) {
	h.mu.RLock()
	c, ok := h.clients[toID]
	h.mu.RUnlock()
	if ok {
		select {
		case c.Send <- data:
		default:
		}
	}
}

func (h *Hub) Broadcast(data []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		select {
		case c.Send <- data:
		default:
		}
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

type APIResp struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

func jsonResp(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func ok(w http.ResponseWriter, data interface{}) { jsonResp(w, 200, APIResp{Success: true, Data: data}) }
func fail(w http.ResponseWriter, status int, msg string) {
	jsonResp(w, status, APIResp{Success: false, Error: msg})
}
func mustMarshal(v interface{}) []byte { b, _ := json.Marshal(v); return b }

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── Auth Handlers ─────────────────────────────────────────────────────────────

// POST /api/auth/register
func registerHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Name     string `json:"name"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, 400, "invalid JSON")
		return
	}
	if body.Username == "" || body.Email == "" || body.Password == "" || body.Name == "" {
		fail(w, 400, "username, email, password, name required")
		return
	}
	if len(body.Password) < 6 {
		fail(w, 400, "password must be at least 6 characters")
		return
	}

	hash, _ := bcrypt.GenerateFromPassword([]byte(body.Password), 12)
	id := genID()
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(
		"INSERT INTO users(id,username,email,password,name,role,created_at) VALUES(?,?,?,?,?,?,?)",
		id, body.Username, body.Email, string(hash), body.Name, body.Role, now,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			fail(w, 409, "username or email already exists")
		} else {
			fail(w, 500, "db error")
		}
		return
	}

	token, _ := signToken(id)
	ok(w, map[string]interface{}{
		"token": token,
		"user": User{ID: id, Username: body.Username, Name: body.Name, Role: body.Role, CreatedAt: now},
	})
}

// POST /api/auth/login
func loginHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Login    string `json:"login"` // username or email
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, 400, "invalid JSON")
		return
	}

	row := db.QueryRow(
		"SELECT id, username, email, password, name, role, bio, avatar_url, created_at FROM users WHERE username=? OR email=?",
		body.Login, body.Login,
	)
	var u User
	var hash string
	err := row.Scan(&u.ID, &u.Username, &u.Email, &hash, &u.Name, &u.Role, &u.Bio, &u.AvatarURL, &u.CreatedAt)
	if err != nil {
		fail(w, 401, "invalid credentials")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Password)) != nil {
		fail(w, 401, "invalid credentials")
		return
	}

	token, _ := signToken(u.ID)
	ok(w, map[string]interface{}{"token": token, "user": u})
}

// GET /api/auth/me
func meHandler(w http.ResponseWriter, r *http.Request) {
	me := uid(r)
	row := db.QueryRow("SELECT id,username,email,name,role,bio,avatar_url,created_at FROM users WHERE id=?", me)
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.Name, &u.Role, &u.Bio, &u.AvatarURL, &u.CreatedAt)
	if err != nil {
		fail(w, 404, "user not found")
		return
	}
	ok(w, u)
}

// PUT /api/auth/profile
func updateProfileHandler(w http.ResponseWriter, r *http.Request) {
	me := uid(r)
	var body struct {
		Name string `json:"name"`
		Role string `json:"role"`
		Bio  string `json:"bio"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	db.Exec("UPDATE users SET name=?,role=?,bio=? WHERE id=?", body.Name, body.Role, body.Bio, me)
	ok(w, map[string]bool{"updated": true})
}

// ── User Handlers ─────────────────────────────────────────────────────────────

// GET /api/users
func getUsersHandler(w http.ResponseWriter, r *http.Request) {
	me := uid(r)
	search := r.URL.Query().Get("q")
	query := `SELECT u.id,u.username,u.name,u.role,u.bio,u.avatar_url,u.online,u.created_at,
		(SELECT COUNT(*) FROM follows WHERE followee_id=u.id) as followers,
		(SELECT COUNT(*) FROM follows WHERE follower_id=? AND followee_id=u.id) as following
		FROM users u WHERE u.id != ?`
	args := []interface{}{me, me}
	if search != "" {
		query += " AND (u.username LIKE ? OR u.name LIKE ? OR u.role LIKE ?)"
		s := "%" + search + "%"
		args = append(args, s, s, s)
	}
	query += " ORDER BY u.online DESC, u.name ASC LIMIT 100"

	rows, err := db.Query(query, args...)
	if err != nil {
		fail(w, 500, "db error")
		return
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		var following int
		rows.Scan(&u.ID, &u.Username, &u.Name, &u.Role, &u.Bio, &u.AvatarURL, &u.Online, &u.CreatedAt, &u.Followers, &following)
		u.Following = following > 0
		users = append(users, u)
	}
	ok(w, users)
}

// GET /api/users/:id
func getUserHandler(w http.ResponseWriter, r *http.Request) {
	me := uid(r)
	targetID := strings.TrimPrefix(r.URL.Path, "/api/users/")
	row := db.QueryRow(`SELECT u.id,u.username,u.name,u.role,u.bio,u.avatar_url,u.online,u.created_at,
		(SELECT COUNT(*) FROM follows WHERE followee_id=u.id) as followers,
		(SELECT COUNT(*) FROM follows WHERE follower_id=? AND followee_id=u.id) as following
		FROM users u WHERE u.id=?`, me, targetID)
	var u User
	var following int
	if err := row.Scan(&u.ID, &u.Username, &u.Name, &u.Role, &u.Bio, &u.AvatarURL, &u.Online, &u.CreatedAt, &u.Followers, &following); err != nil {
		fail(w, 404, "user not found")
		return
	}
	u.Following = following > 0
	ok(w, u)
}

// POST /api/users/:id/follow
func followHandler(w http.ResponseWriter, r *http.Request) {
	me := uid(r)
	target := strings.TrimPrefix(r.URL.Path, "/api/users/")
	target = strings.TrimSuffix(target, "/follow")
	if me == target {
		fail(w, 400, "cannot follow yourself")
		return
	}
	var exists int
	db.QueryRow("SELECT COUNT(*) FROM follows WHERE follower_id=? AND followee_id=?", me, target).Scan(&exists)
	if exists > 0 {
		db.Exec("DELETE FROM follows WHERE follower_id=? AND followee_id=?", me, target)
		ok(w, map[string]bool{"following": false})
	} else {
		db.Exec("INSERT INTO follows(follower_id,followee_id) VALUES(?,?)", me, target)
		ok(w, map[string]bool{"following": true})
	}
}

// ── Message Handlers ──────────────────────────────────────────────────────────

// GET /api/messages?to=
func getMessagesHandler(w http.ResponseWriter, r *http.Request) {
	me := uid(r)
	to := r.URL.Query().Get("to")
	if to == "" {
		fail(w, 400, "to param required")
		return
	}
	rows, err := db.Query(`SELECT id,from_id,to_id,text,code,lang,file_url,file_name,type,created_at
		FROM messages WHERE (from_id=? AND to_id=?) OR (from_id=? AND to_id=?)
		ORDER BY created_at ASC LIMIT 200`, me, to, to, me)
	if err != nil {
		fail(w, 500, "db error")
		return
	}
	defer rows.Close()
	var msgs []Message
	for rows.Next() {
		var m Message
		rows.Scan(&m.ID, &m.FromID, &m.ToID, &m.Text, &m.Code, &m.Lang, &m.FileURL, &m.FileName, &m.Type, &m.CreatedAt)
		msgs = append(msgs, m)
	}
	ok(w, msgs)
}

// POST /api/messages
func sendMessageHandler(w http.ResponseWriter, r *http.Request) {
	me := uid(r)
	var m Message
	json.NewDecoder(r.Body).Decode(&m)
	m.ID = genID()
	m.FromID = me
	m.CreatedAt = time.Now().Format(time.RFC3339)
	if m.Type == "" {
		m.Type = "text"
	}
	db.Exec("INSERT INTO messages(id,from_id,to_id,text,code,lang,file_url,file_name,type,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
		m.ID, m.FromID, m.ToID, m.Text, m.Code, m.Lang, m.FileURL, m.FileName, m.Type, m.CreatedAt)

	payload := mustMarshal(map[string]interface{}{"type": "message", "payload": m})
	hub.SendTo(m.ToID, payload)

	ok(w, m)
}

// ── Post Handlers ─────────────────────────────────────────────────────────────

// GET /api/posts
func getPostsHandler(w http.ResponseWriter, r *http.Request) {
	me := uid(r)
	rows, err := db.Query(`
		SELECT p.id,p.user_id,p.text,p.code,p.lang,p.likes,p.comments,p.created_at,
		u.username,u.name,u.role,u.avatar_url,
		(SELECT COUNT(*) FROM post_likes WHERE user_id=? AND post_id=p.id) as liked
		FROM posts p JOIN users u ON u.id=p.user_id
		ORDER BY p.created_at DESC LIMIT 50`, me)
	if err != nil {
		fail(w, 500, "db error")
		return
	}
	defer rows.Close()
	var posts []Post
	for rows.Next() {
		var p Post
		var u User
		var liked int
		rows.Scan(&p.ID, &p.UserID, &p.Text, &p.Code, &p.Lang, &p.Likes, &p.Comments, &p.CreatedAt,
			&u.Username, &u.Name, &u.Role, &u.AvatarURL)
		rows.Scan(&p.ID, &p.UserID, &p.Text, &p.Code, &p.Lang, &p.Likes, &p.Comments, &p.CreatedAt,
			&u.Username, &u.Name, &u.Role, &u.AvatarURL, &liked)
		p.User = &u
		p.Liked = liked > 0
		posts = append(posts, p)
	}
	ok(w, posts)
}

// POST /api/posts
func createPostHandler(w http.ResponseWriter, r *http.Request) {
	me := uid(r)
	var p Post
	json.NewDecoder(r.Body).Decode(&p)
	if strings.TrimSpace(p.Text) == "" {
		fail(w, 400, "text required")
		return
	}
	p.ID = genID()
	p.UserID = me
	p.CreatedAt = time.Now().Format(time.RFC3339)
	db.Exec("INSERT INTO posts(id,user_id,text,code,lang,created_at) VALUES(?,?,?,?,?,?)",
		p.ID, p.UserID, p.Text, p.Code, p.Lang, p.CreatedAt)

	hub.Broadcast(mustMarshal(map[string]interface{}{"type": "new_post", "payload": p}))
	ok(w, p)
}

// POST /api/posts/:id/like
func likePostHandler(w http.ResponseWriter, r *http.Request) {
	me := uid(r)
	postID := strings.TrimPrefix(r.URL.Path, "/api/posts/")
	postID = strings.TrimSuffix(postID, "/like")

	var exists int
	db.QueryRow("SELECT COUNT(*) FROM post_likes WHERE user_id=? AND post_id=?", me, postID).Scan(&exists)
	if exists > 0 {
		db.Exec("DELETE FROM post_likes WHERE user_id=? AND post_id=?", me, postID)
		db.Exec("UPDATE posts SET likes=MAX(0,likes-1) WHERE id=?", postID)
		ok(w, map[string]bool{"liked": false})
	} else {
		db.Exec("INSERT INTO post_likes(user_id,post_id) VALUES(?,?)", me, postID)
		db.Exec("UPDATE posts SET likes=likes+1 WHERE id=?", postID)
		ok(w, map[string]bool{"liked": true})
	}
}

// ── Upload Handler ────────────────────────────────────────────────────────────

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxUpload)
	if err := r.ParseMultipartForm(MaxUpload); err != nil {
		fail(w, 400, "file too large (max 50MB)")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		fail(w, 400, "missing file")
		return
	}
	defer file.Close()

	name := filepath.Base(header.Filename)
	ext := strings.ToLower(filepath.Ext(name))
	allowed := map[string]bool{
		".go": true, ".py": true, ".ts": true, ".js": true, ".rs": true,
		".java": true, ".c": true, ".cpp": true, ".h": true,
		".json": true, ".yaml": true, ".yml": true, ".toml": true,
		".zip": true, ".tar": true, ".gz": true,
		".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
		".pdf": true, ".md": true, ".txt": true,
	}
	if !allowed[ext] {
		fail(w, 415, "file type not allowed: "+ext)
		return
	}

	os.MkdirAll(UploadDir, 0755)
	dest := filepath.Join(UploadDir, fmt.Sprintf("%d_%s", time.Now().UnixNano(), name))
	out, err := os.Create(dest)
	if err != nil {
		fail(w, 500, "save error")
		return
	}
	defer out.Close()
	io.Copy(out, file)

	ok(w, map[string]string{
		"url":      "/uploads/" + filepath.Base(dest),
		"filename": name,
	})
}

// ── Gemini Handler ────────────────────────────────────────────────────────────

func geminiHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Prompt string `json:"prompt"`
		APIKey string `json:"api_key,omitempty"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	key := body.APIKey
	if key == "" {
		key = GeminiAPIKey
	}
	if key == "" {
		fail(w, 401, "GEMINI_API_KEY not set")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	client, err := genai.NewClient(ctx, option.WithAPIKey(key))
	if err != nil {
		fail(w, 500, "gemini client error: "+err.Error())
		return
	}
	defer client.Close()

	model := client.GenerativeModel("gemini-pro")
	model.SetTemperature(0.7)
	resp, err := model.GenerateContent(ctx, genai.Text(body.Prompt))
	if err != nil {
		fail(w, 502, "gemini error: "+err.Error())
		return
	}

	var sb strings.Builder
	for _, part := range resp.Candidates[0].Content.Parts {
		if txt, ok2 := part.(genai.Text); ok2 {
			sb.WriteString(string(txt))
		}
	}
	ok(w, map[string]string{"reply": sb.String()})
}

// ── WebSocket Handler ─────────────────────────────────────────────────────────

func wsHandler(w http.ResponseWriter, r *http.Request) {
	tokenStr := r.URL.Query().Get("token")
	claims, err := parseToken(tokenStr)
	if err != nil {
		http.Error(w, "unauthorized", 401)
		return
	}
	userID := claims.UserID

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	client := &WSClient{ID: userID, Conn: conn, Send: make(chan []byte, 256)}
	hub.Register(client)
	log.Printf("WS+ %s", userID)

	// writer
	go func() {
		defer func() { conn.Close(); hub.Unregister(userID) }()
		for data := range client.Send {
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if conn.WriteMessage(websocket.TextMessage, data) != nil {
				return
			}
		}
	}()

	// reader
	defer func() { conn.Close(); hub.Unregister(userID); log.Printf("WS- %s", userID) }()
	conn.SetReadLimit(512 * 1024)
	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var msg struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}

		switch msg.Type {
		case "call_signal":
			var sig struct {
				Type string `json:"type"`
				To   string `json:"to"`
				SDP  string `json:"sdp,omitempty"`
				ICE  string `json:"ice,omitempty"`
			}
			json.Unmarshal(msg.Payload, &sig)
			sig2 := map[string]interface{}{"type": sig.Type, "from": userID, "to": sig.To, "sdp": sig.SDP, "ice": sig.ICE}
			hub.SendTo(sig.To, mustMarshal(map[string]interface{}{"type": "call_signal", "payload": sig2}))

		case "typing":
			var t struct{ To string `json:"to"` }
			json.Unmarshal(msg.Payload, &t)
			hub.SendTo(t.To, mustMarshal(map[string]interface{}{"type": "typing", "payload": map[string]string{"from": userID}}))

		case "ping":
			conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		}
	}
}

// ── Health ────────────────────────────────────────────────────────────────────

func healthHandler(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]string{"status": "ok", "service": "Blazvex API", "version": "2.0.0", "time": time.Now().Format(time.RFC3339)})
}

// ── Frontend ──────────────────────────────────────────────────────────────────

func frontendHandler(w http.ResponseWriter, r *http.Request) {
	// Serve static files
	if r.URL.Path != "/" && r.URL.Path != "" {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "Frontend not found. Run: cp index.html static/index.html", 404)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// ── Router ────────────────────────────────────────────────────────────────────

func main() {
	initDB()
	os.MkdirAll(UploadDir, 0755)
	os.MkdirAll("static", 0755)

	mux := http.NewServeMux()

	// Public
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/api/auth/register", registerHandler)
	mux.HandleFunc("/api/auth/login", loginHandler)

	// Protected
	mux.HandleFunc("/api/auth/me", authMiddleware(meHandler))
	mux.HandleFunc("/api/auth/profile", authMiddleware(updateProfileHandler))

	mux.HandleFunc("/api/users", authMiddleware(getUsersHandler))
	mux.HandleFunc("/api/users/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasSuffix(path, "/follow") && r.Method == "POST" {
			authMiddleware(followHandler)(w, r)
		} else {
			authMiddleware(getUserHandler)(w, r)
		}
	})

	mux.HandleFunc("/api/messages", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			authMiddleware(getMessagesHandler)(w, r)
		case "POST":
			authMiddleware(sendMessageHandler)(w, r)
		default:
			fail(w, 405, "method not allowed")
		}
	})

	mux.HandleFunc("/api/posts", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			authMiddleware(getPostsHandler)(w, r)
		case "POST":
			authMiddleware(createPostHandler)(w, r)
		default:
			fail(w, 405, "method not allowed")
		}
	})
	mux.HandleFunc("/api/posts/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/like") && r.Method == "POST" {
			authMiddleware(likePostHandler)(w, r)
		} else {
			fail(w, 404, "not found")
		}
	})

	mux.HandleFunc("/api/upload", authMiddleware(uploadHandler))
	mux.HandleFunc("/api/gemini", authMiddleware(geminiHandler))
	mux.HandleFunc("/ws", wsHandler)

	// Static
	mux.Handle("/uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir(UploadDir))))
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	mux.HandleFunc("/", frontendHandler)

	fmt.Printf(`
  ██████╗ ██╗      █████╗ ███████╗██╗   ██╗███████╗██╗  ██╗
  ██╔══██╗██║     ██╔══██╗╚══███╔╝██║   ██║██╔════╝╚██╗██╔╝
  ██████╔╝██║     ███████║  ███╔╝ ██║   ██║█████╗   ╚███╔╝ 
  ██╔══██╗██║     ██╔══██║ ███╔╝  ╚██╗ ██╔╝██╔══╝   ██╔██╗ 
  ██████╔╝███████╗██║  ██║███████╗ ╚████╔╝ ███████╗██╔╝ ██╗
  ╚═════╝ ╚══════╝╚═╝  ╚═╝╚══════╝  ╚═══╝  ╚══════╝╚═╝  ╚═╝
  Social Messenger for Programmers v2.0 — http://localhost:%s
`+"\n", Port)

	log.Fatal(http.ListenAndServe(":"+Port, cors(mux)))
}
