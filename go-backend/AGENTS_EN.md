# AGENT_EN.md — Collaboration Guide for go-backend

> Purpose: help coding agents and contributors evolve this Go backend safely, without breaking API compatibility, storage compatibility, or operational stability.

---

## 1) Project Contract

This directory is a Go equivalent of the original Python backend.
Priorities (in order):

1. API compatibility for frontend clients
2. SQLite/data compatibility
3. File layout compatibility

Default strategy: **small, reviewable, reversible changes**.

---

## 2) Layer Boundaries

- `internal/api/*`: HTTP layer (routes, auth, status mapping, payloads)
- `internal/schemas/*`: request/response schemas and validation (422 shape)
- `internal/repository/*`: SQLite access and transaction semantics
- `internal/storage/*`: file/chunk I/O only
- `internal/config/*`: environment loading and normalization
- `internal/apperr/*`: shared error types and stable error codes

Do not leak responsibilities across layers.

---

## 3) Error Handling (Go-native)

- Never branch business logic on localized error strings.
- Use:
  - `errors.Is` for sentinel errors
  - `errors.As` for typed errors
  - `apperr.ValueError.Code` for stable classifications
- Human-readable messages can stay localized; control flow must use type/code.
- Keep API error output unified through `writeError(...)`.

---

## 4) Security Rules

### Admin API

- Must be strictly authenticated.
- Current model: `Authorization: Bearer <token>`.
- If `SUPER_CLIPBOARD_ADMIN_API_KEY` is empty, admin endpoints should not be discoverable (return 404).
- Auth failure returns 401 + `WWW-Authenticate`.

### General

- Do not log secrets (captcha secrets, admin key, tokens).
- Validate all external input before repository/storage operations.
- Keep path traversal protections for static/file operations.

---

## 5) Concurrency and Consistency

- Keep repository locks around short DB critical sections only.
- Never hold global locks during slow file I/O.
- For upload session state transitions, prefer explicit CAS-style SQL predicates.
- Error paths must remain recoverable (retry/resume/rollback semantics).

---

## 6) API Compatibility Policy

Before changing behavior, verify:

- path + method
- status code mapping
- response JSON field names/types
- error payload format (`detail`)

Prefer additive changes over breaking changes.

---

## 7) Schema/Migration Policy

- Use safe additive migrations (`IF NOT EXISTS`, guarded `ALTER`).
- Avoid requiring manual migration steps.
- Evaluate indexes for write/read trade-offs.

---

## 8) Testing Expectations

Each change should include/adjust tests covering:

1. happy path
2. unauthorized/invalid input path
3. boundary or race-adjacent path (where practical)

For admin APIs, always test: disabled, unauthorized, authorized success, and not-found/failure paths.

---

## 9) Pre-merge Checklist

1. `gofmt -w` on modified Go files
2. `go test ./...` passes
3. manual endpoint sanity check if behavior changed
4. no secret leakage in logs/responses
5. docs updated when needed

---

## 10) Anti-patterns to Avoid

- `strings.Contains(err.Error(), "...")` as control flow
- embedding SQL-heavy logic in handlers
- bypassing repository with ad-hoc global mutable state
- exposing admin/debug endpoints without strict auth
- only testing happy paths

---

## 11) Change Style Recommendations

- Keep PRs small and focused.
- Explain **why** in comments, not just **what**.
- Reuse existing helpers and conventions.
- Prefer isolated new files for new capabilities (e.g. `admin.go`).
