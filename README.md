# ⚡ BLAZVEX v2.0 — Full-Stack Dev Messenger

A complete real-time messenger for developers with registration, JWT auth, voice & video calls, code sharing, AI assistant, and social feed.

---

## 🚀 Features

- 🔐 **Registration & Login** — JWT authentication, bcrypt passwords
- 💬 **Real-time Chat** — WebSocket, typing indicators, message history
- 📞 **Voice Calls** — WebRTC peer-to-peer audio
- 📹 **Video Calls** — WebRTC peer-to-peer video
- 📎 **File Sharing** — Upload ZIP, code, images (up to 50MB)
- 🖥️ **Code Snippets** — Syntax highlighting (Go, Python, JS, TS, Rust, CSS)
- 😄 **Stickers** — Developer emoji stickers
- 🤖 **Gemini AI** — Code review and debugging assistant
- 🏠 **Social Feed** — Share posts with code snippets, likes
- 🔍 **Explore** — Find and follow other developers
- 🌙 **Dark mode** — Dark navy blue UI
- 💾 **SQLite DB** — Persistent storage (upgradeable to PostgreSQL)

---

## 🛠️ Local Development

### Prerequisites
- Go 1.21+
- GCC (for SQLite CGo)

### Run locally

```bash
git clone https://github.com/YOUR_USERNAME/blazvex
cd blazvex

# Install dependencies
go mod tidy

# Set environment variables (optional)
export JWT_SECRET="your-secret-key"
export GEMINI_API_KEY="AIza..."   # optional

# Run
go run main.go
# → http://localhost:8080
```

---

## 🌐 Deploy to Render.com (Free Hosting)

### Step 1 — Push to GitHub

```bash
cd blazvex
git init
git add .
git commit -m "Initial commit: Blazvex v2.0"
git branch -M main

# Create repo on GitHub first at https://github.com/new
git remote add origin https://github.com/YOUR_USERNAME/blazvex.git
git push -u origin main
```

### Step 2 — Deploy on Render

1. Go to **https://render.com** → Sign up / Log in
2. Click **"New +"** → **"Web Service"**
3. Connect your GitHub account → Select the `blazvex` repository
4. Render will auto-detect the `render.yaml` — click **"Apply"**
5. OR configure manually:
   - **Runtime:** Docker
   - **Dockerfile Path:** `./Dockerfile`
   - **Plan:** Free

### Step 3 — Set Environment Variables in Render

In your Render service → **Environment** tab, add:

| Key | Value |
|-----|-------|
| `JWT_SECRET` | any long random string (e.g. `openssl rand -hex 32`) |
| `GEMINI_API_KEY` | your Google Gemini key (optional) |
| `PORT` | `8080` |
| `DB_PATH` | `/data/blazvex.db` |
| `UPLOAD_DIR` | `/data/uploads` |

### Step 4 — Add Persistent Disk (Important!)

1. In your Render service → **Disks** tab
2. Add disk: **Name:** `blazvex-data`, **Mount Path:** `/data`, **Size:** 1 GB
3. This keeps your database & uploads between deploys

### Step 5 — Deploy!

- Click **"Deploy Latest Commit"**
- Wait ~3-5 minutes for Docker build
- Get your URL: `https://blazvex-xxxx.onrender.com`

---

## 📡 API Reference

### Auth
| Method | Endpoint | Body | Description |
|--------|----------|------|-------------|
| POST | `/api/auth/register` | `{username, email, password, name, role}` | Register |
| POST | `/api/auth/login` | `{login, password}` | Login |
| GET | `/api/auth/me` | — | My profile |
| PUT | `/api/auth/profile` | `{name, role, bio}` | Update profile |

### Users
| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/users` | List users (supports `?q=search`) |
| GET | `/api/users/:id` | Get user |
| POST | `/api/users/:id/follow` | Follow/unfollow |

### Messages
| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/messages?to=:id` | Get conversation |
| POST | `/api/messages` | Send message |

### Posts
| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/posts` | Get feed |
| POST | `/api/posts` | Create post |
| POST | `/api/posts/:id/like` | Like/unlike |

### Other
| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/upload` | Upload file |
| POST | `/api/gemini` | Ask Gemini AI |
| WS | `/ws?token=JWT` | WebSocket |

---

## 💬 WebSocket Events

```json
// Receive new message
{"type": "message", "payload": {...Message}}

// Receive typing indicator
{"type": "typing", "payload": {"from": "user_id"}}

// User online/offline
{"type": "presence", "payload": {"user_id": "...", "online": true}}

// WebRTC call signal
{"type": "call_signal", "payload": {"type": "offer|answer|candidate|hangup", "from": "...", "to": "..."}}
```

---

## 🏗️ Architecture

```
blazvex/
├── main.go          # Go backend — HTTP + WebSocket + WebRTC signaling
├── static/
│   └── index.html   # Single-file React-less frontend
├── go.mod / go.sum  # Go dependencies
├── Dockerfile       # Docker build for Render
├── render.yaml      # Render.com deploy config
└── .gitignore
```

**Stack:** Go 1.21 · SQLite (go-sqlite3) · JWT (golang-jwt) · bcrypt · gorilla/websocket · Google Gemini · Vanilla JS · WebRTC · JetBrains Mono + Syne fonts

---

## 🔑 Get Gemini API Key

1. Go to https://aistudio.google.com/app/apikey
2. Create key
3. Add to Render env vars as `GEMINI_API_KEY`
4. Or set it in the app UI under the Gemini panel

---

## 🔧 Upgrade to PostgreSQL (optional)

Replace SQLite with PostgreSQL for production scale:

1. Add `"github.com/lib/pq"` to go.mod
2. Change `sql.Open("sqlite3", ...)` to `sql.Open("postgres", os.Getenv("DATABASE_URL"))`
3. Update SQL syntax (`?` → `$1`, `$2`, etc.)
4. Add a Render PostgreSQL database in the Render dashboard
