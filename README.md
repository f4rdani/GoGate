# 🚀 GoGate

**OpenAI-compatible multi-provider AI proxy / Gateway** — ringan, cepat, dan mudah dikonfigurasi.

Satu gateway untuk mengakses **60+ model AI** dari berbagai provider (OpenAI, Anthropic, Groq, Mistral, dll) dengan **satu API key** dan **satu endpoint**.

---

## ✨ Fitur

| Fitur | Deskripsi |
|-------|-----------|
| 🔀 **Multi-Provider** | OpenAI, Anthropic, Groq, Mistral, DeepSeek, OpenRouter, dan custom |
| 🎯 **3-Tier Smart Fallback** | Otomatis fallback: subscription → cheap → free |
| ⚖️ **Round-Robin Load Balancing** | Distribusi request merata ke beberapa provider |
| 💾 **Response Cache** | Cache response non-streaming (LRU + TTL) untuk hemat cost |
| 🛡️ **Token Saver** | Kompresi tool output hemat 20-40% token input |
| 📊 **Usage Tracking** | Lacak penggunaan token per provider, model, dan API key |
| 🏥 **Health Check** | Monitor kesehatan provider secara otomatis |
| 🔄 **Retry with Backoff** | Retry otomatis dengan exponential backoff + jitter |
| ⚡ **Circuit Breaker** | Backend error auto-disabled 30 detik, auto-recovery, skip ke backend lain |
| 🚦 **Rate Limiting & Queuing** | Per-provider, per-model, dengan request queuing |
| 🔑 **API Key Management** | Per-user key dengan model access control |
| ⚡ **Hot Reload** | Reload config tanpa restart server |
| 🎮 **Interactive CLI** | TUI interaktif (arrow-key, spinner, colors) untuk manage semua konfigurasi |
| 🌐 **Playground** | Web UI untuk test model langsung dari browser |
| 🔐 **Admin API** | REST API untuk manage keys dan monitor gateway |
| 🌐 **Admin Dashboard** | Web dashboard full CRUD — manage providers, models, keys, config dari browser |
| 🧪 **Web Diagnostics** | Test API key, test model, sync models, quick setup langsung dari dashboard (mendukung lencana 🧠 & 👁️) |
| 🔄 **Simultaneous Mode** | Server + Terminal CLI jalan bersamaan — config dari mana saja |
| 👁️ **Vision (Multimodal)** | Dukungan input gambar/multi-part (termasuk auto-translate format OpenAI `image_url` ke format native block Anthropic/Claude) |
| 🔢 **Embeddings Support** | Proksi endpoint `/v1/embeddings` dengan load balancing, fallback, dan retry yang sama |
| 🛡️ **Security Hardened** | Proteksi SSRF (blokir IP privat/lokal), validasi API key tahan timing-attack (SHA-256), pembatasan CORS admin, dan isolasi sesi |

---

## 📦 Instalasi

### Prerequisites

- **Go 1.22+** — [Download](https://go.dev/dl/)
- **Git** (opsional, untuk clone)

### 1. Clone Repository

```bash
git clone https://github.com/f4rdani/GoGate.git
cd GoGate
```

### 2. Install Dependencies

```bash
go mod download
```

### 3. Build

```bash
# Build binary
go build -o aigateway.exe .

# Atau langsung run tanpa build
go run .
```

### 4. Setup Config

Copy dan edit config:

```bash
# Config sudah tersedia, edit sesuai kebutuhan
# Buka config.yaml di text editor favorit kamu
```

---

## ⚙️ Konfigurasi

### Struktur `config.yaml`

```yaml
server:
    host: 0.0.0.0          # Listen address
    port: 8080              # Listen port
    admin_secret: your-secret  # Secret untuk admin API
    log_level: info         # debug | info | warn | error

concurrency:
    max_concurrent: 100     # Max request bersamaan global
    per_provider: 30        # Max per provider
    per_model: 15           # Max per model
    queue_depth: 50         # Max antrian saat slot penuh
    queue_timeout: 30s      # Max waktu tunggu di antrian

cache:
    enabled: true           # Aktifkan response cache
    max_size: 1000          # Max entry cache
    ttl: 300                # TTL cache dalam detik

retry:
    max_retries: 2          # Max retry per backend
    initial_backoff: 500    # Backoff awal (ms)
    max_backoff: 10000      # Backoff maksimal (ms)
```

### Menambah Provider

```yaml
providers:
    - name: openai
      type: openai                          # openai | anthropic | groq | mistral | custom
      base_url: https://api.openai.com/v1
      tier: 1                               # 1=subscription, 2=cheap, 3=free
      health_check_interval: 60s            # Interval health check (0=disabled)
      api_keys:
        - ${OPENAI_API_KEY}                 # Support env variable
      models:
        - gpt-4o
        - gpt-4o-mini
        - gpt-4.1
```

### Mendefinisikan Model

```yaml
models:
    # Model direct — satu provider
    - name: gpt-4o
      provider: openai
      model: gpt-4o

    # Round-Robin — rotasi ke beberapa provider
    - name: fast-mix
      strategy: round-robin
      backends:
        - provider: groq
          model: llama-3.3-70b-versatile
        - provider: mistral
          model: mistral-small-latest

    # Smart Fallback — tier 1 → 2 → 3
    - name: smart-fallback
      strategy: fallback
      backends:
        - provider: openai
          model: gpt-4o
          tier: 1
        - provider: anthropic
          model: claude-sonnet-4-20250514
          tier: 1
        - provider: groq
          model: llama-3.3-70b-versatile
          tier: 2
        - provider: neosan
          model: glm-4.7-flash
          tier: 3
```

### API Key (User/Client)

```yaml
api_keys:
    - key: sk-gw-admin-your-key
      name: Admin Full Access
      allowed_models:
        - '*'                # Akses semua model
      rate_limit: 0          # 0 = unlimited

    - key: sk-gw-user-your-key
      name: User 1 - Budget
      allowed_models:
        - fast-mix
        - gpt-4o-mini
      rate_limit: 60         # 60 request per menit
```

### Environment Variables

Gunakan `${VAR_NAME}` di config untuk inject dari environment:

```bash
export OPENAI_API_KEY="sk-..."
export ANTHROPIC_API_KEY="sk-ant-..."
export GROQ_API_KEY="gsk-..."
```

---

## 🚀 Cara Pakai

### Mode 1: Server + Terminal Bersamaan (Recommended)

```bash
# Start server + terminal config sekaligus
./aigateway.exe
```

Server berjalan di background di `http://localhost:8080`, sambil terminal menampilkan menu interaktif. Kamu bisa config dari **terminal** atau dari **web dashboard** (`http://localhost:8080/admin`) — keduanya bisa dipakai bersamaan!

Logs disimpan ke `aigateway.log` supaya tidak corrupt TUI.

### Mode 2: Server Only

```bash
# Server saja tanpa terminal
./aigateway.exe serve

# Dengan config custom
./aigateway.exe serve -config my-config.yaml
```

Server berjalan di `http://localhost:8080`. Logs tampil di stdout.

### Mode 3: Interactive CLI Only (Tanpa Server)

```bash
./aigateway.exe config
```

CLI interaktif berbasis [charmbracelet/huh](https://github.com/charmbracelet/huh) dengan navigasi arrow-key, warna, dan spinner:

```
  🚀  AI Gateway — Config CLI

  ┌─────────────────────────────────────────────────┐
  │ 📁 Config: config.yaml                          │
  │ 📡 Providers: 5  🔑 API Keys: 2  🤖 Models: 12 │
  └─────────────────────────────────────────────────┘

    Pilih Menu
    > 🤖  Manajemen AI (Provider/Model/Combo)
      ⚙️   Manajemen Server
      🔑  Manajemen API Key (User/Client)
      🧪  Diagnostik & Testing
      📋  Lihat Summary
      💾  Simpan & Keluar
```

**Navigasi:** `↑↓` arrow keys untuk pilih, `Enter` untuk select, `Esc` untuk kembali.

#### Fitur CLI Interaktif

| Menu | Fitur |
|------|-------|
| **🤖 Manajemen AI** | Quick Setup dengan spinner, multi-select model, password input untuk API key |
| **⚙️ Server Settings** | Edit host/port, admin secret (masked), concurrency limits, log level |
| **🔑 API Key** | Buat/edit/hapus key, multi-select model access, rate limit per-user |
| **🧪 Diagnostik** | Test API key (spinner + badge), test model (latency), sync models dari provider |
| **📋 Summary** | Ringkasan config dengan styled output: provider badges, key-value pairs |

#### Quick Setup Flow

```
  ⚡ Quick Setup — Tambah Provider

  Pilih provider
  > openai           OpenAI — GPT-4o, GPT-4.1, dll
    anthropic        Anthropic — Claude Sonnet, Haiku
    groq             Groq — Llama, Mixtral (super cepat)
    mistral          Mistral AI — Mistral Large, Small
    deepseek         DeepSeek — Chat, Reasoner
    openrouter       OpenRouter — akses 300+ model
    🔧  Provider lain (custom)

  ⠋ Mengambil model dari provider...    ← Animated spinner

  Pilih model (space untuk select, enter untuk confirm)
  > [x] gpt-4o
    [x] gpt-4o-mini
    [ ] gpt-4.1
    [x] gpt-4.1-mini

  ✅ Provider 'openai' ditambahkan!
     📡 1 API key  │  🤖 4 model  │  🛤  4 route dibuat

  Buat API key user untuk akses model ini?
  > Yes / No
```

#### Test API Key Flow

```
  🔑 Test API Key

  Pilih provider yang mau di-test
  > openai [openai] — 4 models
    anthropic [anthropic] — 2 models
    groq [groq] — 2 models

  ⠋ Testing API key openai...          ← Animated spinner

  ┌────────┐
  │ VALID  │                            ← Styled badge
  └────────┘
  Model tersedia: 15 model
```

#### Warna & Styling

CLI menggunakan [charmbracelet/lipgloss](https://github.com/charmbracelet/lipgloss) untuk:
- **Color palette** — Purple (primary), Blue (secondary), Green (accent)
- **Provider badges** — Warna per tipe (OpenAI=hijau, Anthropic=kuning, Groq=ungu, Mistral=biru)
- **Status badges** — `VALID` (hijau), `BERFUNGSI` (hijau), `GAGAL` (merah)
- **Key-value pairs** — Label bold putih, value light gray
- **Spinner** — Animated braille (`⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏`) saat loading

CLI menyediakan menu interaktif untuk:
- **Quick Setup** — Tambah provider (OpenAI, Anthropic, Groq, dll) dengan wizard interaktif
- **Auto-fetch models** — Otomatis ambil daftar model dari API provider (dengan spinner)
- **Multi-select** — Pilih beberapa model sekaligus dengan space bar
- **Password input** — API key diinput tanpa terlihat di terminal
- **Kelola API Keys** — Buat, edit, hapus user API key dengan model access control
- **Combo Models** — Buat model dengan round-robin atau fallback strategy
- **Diagnostik** — Test API key dan model langsung dari CLI (dengan badge hasil)
- **Hot Reload** — Config tersimpan otomatis dan server reload tanpa restart
- **Confirm dialogs** — Konfirmasi sebelum hapus provider/key/model

### Mode 4: Development (Go Run)

```bash
go run .              # Start server + terminal sekaligus
go run . serve        # Server only
go run . config       # Terminal only
```

---

## 📡 Menggunakan Gateway

Gateway ini **100% compatible** dengan OpenAI API format. Cukup ganti `base_url` di client SDK kamu.

### cURL

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-gw-your-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```

### Python (openai SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="sk-gw-your-key"
)

response = client.chat.completions.create(
    model="smart-fallback",  # Gunakan combo model
    messages=[{"role": "user", "content": "Hello!"}]
)
print(response.choices[0].message.content)
```

### JavaScript/TypeScript

```javascript
import OpenAI from 'openai';

const client = new OpenAI({
  baseURL: 'http://localhost:8080/v1',
  apiKey: 'sk-gw-your-key',
});

const response = await client.chat.completions.create({
  model: 'fast-mix',
  messages: [{ role: 'user', content: 'Hello!' }],
  stream: true,
});

for await (const chunk of response) {
  process.stdout.write(chunk.choices[0]?.delta?.content || '');
}
```

### Streaming

Streaming didukung penuh. Setiap chunk SSE dikirim langsung dari provider ke client.

### Mendapatkan Daftar Model

```bash
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer sk-gw-your-key"
```

---

## 🔐 Admin API

Gunakan header `X-Admin-Secret` untuk akses admin endpoints.

### Reload Config (Hot Reload)

```bash
curl -X POST http://localhost:8080/admin/config/reload \
  -H "X-Admin-Secret: your-admin-secret"
```

### Lihat Semua API Key

```bash
curl http://localhost:8080/admin/keys \
  -H "X-Admin-Secret: your-admin-secret"
```

### Test API Key (Diagnostik)

```bash
curl -X POST http://localhost:8080/admin/diag/test-key \
  -H "X-Admin-Secret: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"base_url": "https://api.openai.com/v1", "api_key": "sk-...", "type": "openai"}'
```

### Test Model (Diagnostik)

```bash
curl -X POST http://localhost:8080/admin/diag/test-model \
  -H "X-Admin-Secret: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"base_url": "https://api.openai.com/v1", "api_key": "sk-...", "type": "openai", "model": "gpt-4o"}'
```

### Fetch Models dari Provider

```bash
curl -X POST http://localhost:8080/admin/diag/fetch-models \
  -H "X-Admin-Secret: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"base_url": "https://api.openai.com/v1", "api_key": "sk-...", "type": "openai"}'
```

### Quick Setup (Template Provider)

```bash
# Lihat templates
curl http://localhost:8080/admin/templates \
  -H "X-Admin-Secret: your-admin-secret"

# Setup dari template
curl -X POST http://localhost:8080/admin/templates/setup \
  -H "X-Admin-Secret: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"template": "openai", "api_key": "sk-..."}'
```

### Lihat Semua API Key

```bash
curl http://localhost:8080/admin/keys \
  -H "X-Admin-Secret: your-admin-secret"
```

### Buat API Key Baru

```bash
curl -X POST http://localhost:8080/admin/keys \
  -H "X-Admin-Secret: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "New User",
    "allowed_models": ["gpt-4o", "fast-mix"],
    "rate_limit": 30
  }'
```

### Hapus API Key

```bash
curl -X DELETE http://localhost:8080/admin/keys/sk-gw-the-key \
  -H "X-Admin-Secret: your-admin-secret"
```

### Health Status

```bash
curl http://localhost:8080/v1/health
```

---

## 🏗️ Arsitektur

```
Client Request
     │
     ▼
┌─────────────┐
│   Router     │ ← Auth + Rate Limit + Queue
│  (proxy)     │
└──────┬──────┘
       │
       ▼
┌─────────────┐     ┌──────────────┐
│  Strategy    │────→│ Round-Robin  │ ← Rotasi provider
│  Selector    │     └──────────────┘
│              │     ┌──────────────┐
│              │────→│  Fallback    │ ← Tier 1→2→3
│              │     └──────────────┘
│              │     ┌──────────────┐
│              │────→│   Direct     │ ← Langsung ke provider
└──────┬──────┘     └──────────────┘
       │
       ▼
┌─────────────┐     ┌──────────────┐
│  Provider    │────→│ Health Check │ ← Skip unhealthy
│  Client      │     └──────────────┘
└──────┬──────┘     ┌──────────────┐
       │────────────→│ Circuit Break│ ← Disable 30s saat error
       │             └──────────────┘
       │             ┌──────────────┐
       │────────────→│   Retry      │ ← Exponential backoff
       │             └──────────────┘
       ▼
┌─────────────┐     ┌──────────────┐
│  Response    │────→│    Cache     │ ← LRU + TTL
│  Handler     │     └──────────────┘
└──────┬──────┘     ┌──────────────┐
       │────────────→│ Token Saver  │ ← Kompresi output
       │             └──────────────┘
       │             ┌──────────────┐
       │────────────→│   Usage      │ ← Track token usage
       │             └──────────────┘
       ▼
   Client Response
```

---

## 📊 Perbandingan dengan Alternatif

| Aspek | AI Gateway (Go) | 9router (Node.js) | LiteLLM (Python) |
|-------|-----------------|-------------------|-------------------|
| **Bahasa** | Go | Node.js | Python |
| **RAM Usage** | <30MB | ~150MB | ~200MB |
| **Concurrency** | 10,000+ req/s | ~1,000 req/s | ~500 req/s |
| **Binary Size** | ~15MB | ~200MB+ | ~500MB+ |
| **Dependencies** | 2 (yaml, uuid) | 50+ | 100+ |
| **Startup Time** | <100ms | ~2s | ~3s |
| **Smart Fallback** | ✅ 3-Tier | ✅ | ✅ |
| **Response Cache** | ✅ LRU+TTL | ❌ | ✅ |
| **Token Saver** | ✅ | ✅ | ❌ |
| **Health Check** | ✅ | ❌ | ✅ |
| **Request Queuing** | ✅ | ❌ | ❌ |
| **Interactive CLI** | ✅ | ❌ | ❌ |
| **Web Dashboard** | ✅ Full CRUD | ❌ | ❌ |
| **Web Diagnostics** | ✅ | ❌ | ❌ |

---

## 🛠️ Development

### Project Structure

```
aigateway/
├── main.go                 # Entry point
├── config/
│   ├── config.go           # Config struct + loader
│   └── config_manager.go   # Config CRUD operations
├── server/
│   └── server.go           # HTTP server + wiring
├── router/
│   └── router.go           # Routing strategy (direct/rr/fallback)
├── proxy/
│   └── handler.go          # Request proxy handler
├── provider/
│   ├── provider.go         # Provider interface + registry
│   ├── openai.go           # OpenAI-compatible provider
│   ├── anthropic.go        # Anthropic provider
│   ├── groq.go             # Groq provider
│   ├── mistral.go          # Mistral provider
│   └── custom.go           # Custom provider
├── balancer/
│   └── roundrobin.go       # Round-robin load balancer
├── cache/
│   └── lru.go              # LRU cache with TTL
├── usage/
│   └── tracker.go          # Token usage tracker
├── middleware/
│   ├── ratelimit.go        # Rate limiting + queuing
│   ├── logging.go          # Request logging
│   └── tokensaver.go       # Token compression
├── auth/
│   └── auth.go             # API key authentication
├── admin/
│   ├── admin.go            # Admin API handlers + CRUD + diagnostics
│   ├── dashboard.go        # Dashboard embed handler
│   └── dashboard.html      # Full CRUD web dashboard (single file, zero deps)
├── models/
│   ├── openai.go           # OpenAI request/response models
│   └── anthropic.go        # Anthropic request/response models
├── playground/
│   ├── handler.go          # Playground web handler
│   └── index.html          # Playground UI
├── cli/
│   ├── cli.go              # Interactive CLI main (huh TUI)
│   ├── style.go            # Lipgloss styles, spinner, color palette
│   ├── menu_ai.go          # AI management menu (provider/model)
│   ├── menu_server.go      # Server settings menu
│   ├── menu_api_keys.go    # API key management menu
│   ├── menu_diag.go        # Diagnostics menu (with spinners)
│   └── provider_client.go  # Provider API client
├── config.yaml             # Configuration file
├── go.mod
└── go.sum
```

### Build & Test

```bash
# Build
go build -o aigateway.exe .

# Run tests
go test ./... -v

# Vet (static analysis)
go vet ./...

# Run specific package tests
go test ./cache/... -v
go test ./router/... -v
go test ./usage/... -v
```

---

## 📝 Changelog

### v2.1 — Interactive CLI (TUI)
- ✅ Charmbracelet/huh interactive menus (arrow-key navigation)
- ✅ Lipgloss styling (color palette, badges, key-value pairs)
- ✅ Animated braille spinner untuk async operations
- ✅ Multi-select untuk model selection
- ✅ Password input untuk API key
- ✅ Confirm dialogs untuk delete operations
- ✅ Provider type badges (warna per tipe)
- ✅ Panic-safe spinner dengan recovery

### v2.3 — Full CRUD Dashboard + Diagnostics
- ✅ Web dashboard full CRUD: manage providers, models, API keys, config dari browser
- ✅ Dynamic modal system untuk semua form (add/edit/delete)
- ✅ Diagnostic endpoints: test API key, test model, fetch models, quick setup templates
- ✅ Web diagnostics UI: 4 clickable cards (Quick Setup, Test Key, Test Model, Sync Models)
- ✅ Simultaneous server + terminal mode (`aigateway` = server + TUI)
- ✅ Logs redirect ke file saat TUI running (prevent output corruption)
- ✅ Partial update: edit provider tidak hapus API keys/models yang sudah ada
- ✅ Cascade delete: hapus provider otomatis bersihkan routes, combos, AllowedModels
- ✅ XSS safe: semua dynamic content di-escape dengan `esc()`
- ✅ Dashboard ~45KB single file, zero dependencies

### v2.2 — Circuit Breaker
- ✅ Auto-disable backend selama 30 detik saat error (429, 5xx, connection error, dll)
- ✅ Round-robin auto-fallback: skip backend yang disabled, coba backend berikutnya
- ✅ Fallback/tiered circuit breaker: skip backend yang disabled, lanjut ke tier berikutnya
- ✅ Anti-starvation: jika semua backend disabled, tetap coba yang awal dipilih
- ✅ Auto-recovery: backend otomatis tersedia lagi setelah 30 detik
- ✅ Thread-safe: lock-free pakai `atomic.Int64`
- ✅ Config tanpa API key tidak blokir provider lain

### v2.0 — 9Router Feature Parity
- ✅ 3-Tier Smart Fallback routing
- ✅ Response cache (LRU + TTL)
- ✅ Token Saver compression (20-40% savings)
- ✅ Usage tracking per provider/model/key
- ✅ Provider health checks with auto-recovery
- ✅ Retry with exponential backoff + jitter
- ✅ Request queuing with configurable timeout
- ✅ 66 unit tests

### v1.0 — Initial Release
- Multi-provider proxy (OpenAI, Anthropic, Groq, Mistral, Custom)
- Round-robin and fallback load balancing
- Streaming support
- API key management with per-model access control
- Interactive CLI configuration
- Playground web UI
- Admin API with hot reload
- Rate limiting (per-provider, per-model)

---

## 🌐 Admin Dashboard

Web dashboard tersedia di `http://localhost:8080/admin`. Login dengan `admin_secret` dari config.

### Fitur Dashboard

| Entity | Create | Read | Update | Delete |
|--------|--------|------|--------|--------|
| **Providers** | ✅ Modal form | ✅ Table + health | ✅ Edit base_url/tier | ✅ Cascade delete |
| **Models** | ✅ Direct / Combo | ✅ Table | ✅ Edit strategy/backends | ✅ Cascade delete |
| **API Keys** | ✅ Modal form | ✅ Table | ✅ Edit name/models/rate | ✅ Delete |
| **Config** | — | ✅ Server/Concurrency/Cache/Retry | ✅ Edit semua section | — |

### Diagnostics Dashboard

| Fitur | Deskripsi |
|-------|-----------|
| ⚡ **Quick Setup** | Wizard pilih provider template → input API key → auto-fetch & buat model routes |
| 🔑 **Test API Key** | Verifikasi API key ke provider langsung dari browser |
| 🤖 **Test Model** | Kirim test request ke model, tampilkan response + latency |
| 🔄 **Sync Models** | Fetch daftar model dari provider API, auto-add model routes baru |

---

## 📄 License

MIT License

---

## 🤝 Contributing

1. Fork repository
2. Buat feature branch (`git checkout -b feature/amazing-feature`)
3. Commit changes (`git commit -m 'Add amazing feature'`)
4. Push ke branch (`git push origin feature/amazing-feature`)
5. Buka Pull Request

---

<div align="center">

**Made with ❤️ and Go**

[Report Bug](https://github.com/f4rdani/GoGate/issues) · [Request Feature](https://github.com/f4rdani/GoGate/issues)

</div>
