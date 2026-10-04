import {
  abortFileUpload,
  asUploadHttpError,
  completeUpload,
  getUploadInfo,
  initFileUpload,
  uploadChunkBytes,
  type UploadClipParams,
  type UploadHttpError,
  type UploadInfoResponse
} from "./api";
import type { RemoteClip } from "../store/useClipboardStore";

// ---------------------------------------------------------------------------
// Client half of the CURRENT server protocol.
//
// The server (go-backend) no longer keeps one file per chunk and no longer
// merges anything: INIT knows fileSize + chunkSize, so it pre-allocates ONE
// file at its final path with truncate(fileSize) and every PUT writes its bytes
// directly into the range [index * chunkSize, +chunkSize) with WriteAt. The
// upload_chunks table is the only source of truth for progress, which means:
//
//   * every chunk but the last one MUST carry exactly `chunkSize` bytes -- any
//     other payload is refused with a parameter error (400/413). The client
//     mirrors that contract locally (verifyChunkPayload) and never sends a
//     range the server would have to reject;
//   * COMPLETE has nothing to assemble: no "assembling"/merge phase exists any
//     more, so the progress model only has
//     initializing -> (resuming) -> uploading -> retrying -> finalizing.
//   * progress is resumable from the DB alone: GET /uploads/{id} (and a
//     replayed INIT) answer with receivedChunks/missingChunks, and a lost or
//     resized container answers 409 + `missing` (all ranges to re-send).
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Pure helpers (unit-tested, no DOM/fetch).
// ---------------------------------------------------------------------------

export const calcTotalChunks = (fileSize: number, chunkSize: number): number => {
  if (!Number.isFinite(fileSize) || !Number.isFinite(chunkSize)) return 0;
  if (fileSize <= 0 || chunkSize <= 0) return 0;
  return Math.ceil(fileSize / chunkSize);
};

export const getChunkRange = (
  index: number,
  chunkSize: number,
  fileSize: number
): { start: number; end: number } => {
  const start = index * chunkSize;
  const end = Math.min(start + chunkSize, fileSize);
  return { start, end };
};

// Byte offset of a chunk inside the single pre-allocated file. INIT truncated
// the file to fileSize, so the offset is pure arithmetic -- exactly what the
// server uses for WriteAt.
export const getChunkOffset = (index: number, chunkSize: number): number => {
  if (!Number.isFinite(index) || !Number.isFinite(chunkSize)) return 0;
  return Math.max(0, index) * Math.max(0, chunkSize);
};

// Size the server expects for one chunk: chunkSize for every chunk, the
// remainder for the last one.
export const expectedChunkSize = (
  index: number,
  chunkSize: number,
  fileSize: number
): number => {
  const start = getChunkOffset(index, chunkSize);
  if (fileSize <= start) return 0;
  return Math.min(chunkSize, fileSize - start);
};

// Sanity-check the server-generated plan against the local file. A mismatch
// means the two sides disagree about the byte layout, so uploading would either
// be refused per chunk (size mismatch) or leave a partially written file.
export const verifyChunkPlan = (plan: {
  fileSize: number;
  chunkSize: number;
  totalChunks: number;
}): string | null => {
  const { fileSize, chunkSize, totalChunks } = plan;
  if (!Number.isFinite(fileSize) || fileSize < 0) {
    return "文件大小无效";
  }
  if (!Number.isFinite(totalChunks) || totalChunks < 0) {
    return "服务端返回的分片数量无效";
  }
  if (fileSize === 0) {
    return totalChunks === 0 ? null : "服务端返回的分片计划与文件大小不一致";
  }
  if (!Number.isFinite(chunkSize) || chunkSize <= 0) {
    return "服务端返回的分片大小无效";
  }
  if (totalChunks !== calcTotalChunks(fileSize, chunkSize)) {
    return "服务端返回的分片数量与文件大小不一致";
  }
  return null;
};

// Local mirror of the server-side size contract; the message wording matches
// the server's 400 so both paths read the same in the UI.
export const verifyChunkPayload = (
  index: number,
  payloadBytes: number,
  chunkSize: number,
  fileSize: number
): string | null => {
  const expected = expectedChunkSize(index, chunkSize, fileSize);
  if (payloadBytes === expected) return null;
  return `分片大小不匹配：第 ${index} 块应为 ${expected} 字节，实际 ${payloadBytes} 字节`;
};

export const normalizeIndices = (values: unknown, totalChunks?: number): number[] => {
  if (!Array.isArray(values)) return [];
  const seen = new Set<number>();
  for (const value of values) {
    if (typeof value !== "number" || !Number.isInteger(value) || value < 0) continue;
    if (totalChunks !== undefined && value >= totalChunks) continue;
    seen.add(value);
  }
  return [...seen].sort((a, b) => a - b);
};

export const getMissingIndices = (
  totalChunks: number,
  received: number[]
): number[] => {
  if (totalChunks <= 0) return [];
  const seen = new Set(received);
  const out: number[] = [];
  for (let i = 0; i < totalChunks; i += 1) {
    if (!seen.has(i)) out.push(i);
  }
  return out;
};

// Resume state is rebuilt from the server answer, never from local guesses: the
// DB owns progress, this just projects it into received/missing lists.
export const deriveChunkState = (
  info: UploadInfoResponse
): { received: number[]; missing: number[] } => {
  const total = Number.isFinite(info.totalChunks) ? Math.max(0, info.totalChunks) : 0;
  const received = normalizeIndices(info.receivedChunks, total);
  const reported = Array.isArray(info.missingChunks)
    ? normalizeIndices(info.missingChunks, total)
    : null;
  return {
    received,
    missing:
      reported && reported.length > 0 ? reported : getMissingIndices(total, received)
  };
};

export const backoffDelayMs = (attempt: number): number => {
  // Exponential backoff with jitter: 400ms, 800ms, 1600ms... capped at 5s.
  const base = 400 * Math.pow(2, Math.max(0, attempt - 1));
  const capped = Math.min(base, 5000);
  const jitter = Math.floor(Math.random() * 150);
  return capped + jitter;
};

export const buildResumeKey = (params: {
  environmentId: string;
  filename: string;
  fileSize: number;
  lastModified: number;
  // Clip params are frozen server-side at init, so the resume slot must be
  // keyed by the access mode too: switching from a short code to a token (or
  // changing the code) has to start a NEW session instead of replaying one
  // whose frozen params can never succeed again.
  accessCode?: string;
  accessToken?: string;
}): string => {
  const { environmentId, filename, fileSize, lastModified } = params;
  const access = `${params.accessCode ?? ""}|${params.accessToken ? "token" : ""}`;
  return `super-clipboard::upload:${environmentId}::${filename}::${fileSize}::${lastModified}::${access}`;
};

export type ChunkFailureKind =
  // 409 + `missing`: the byte container was lost/resized and every receipt was
  // dropped, so the named ranges must be sent again.
  | "reset"
  // 409 without `missing`: the session is `completing`/`completed` (or a failed
  // complete rolled it back). The probe decides what to do.
  | "session"
  // 404/410: the session row is gone -> re-init.
  | "gone"
  // Parameter/auth errors (400/401/403/413/422...): retrying cannot help.
  | "fatal"
  // 408/429/5xx (incl. 507 storage guards) and network drops.
  | "retryable";

export const classifyChunkFailure = (error: unknown): ChunkFailureKind => {
  const { status, missing } = asUploadHttpError(error);
  if (status === undefined) {
    // No HTTP status: a network drop or our own chunk timeout -> retryable.
    return "retryable";
  }
  if (status === 409) {
    return Array.isArray(missing) && missing.length > 0 ? "reset" : "session";
  }
  if (status === 404 || status === 410) return "gone";
  if (status === 408 || status === 429 || status >= 500) return "retryable";
  return "fatal";
};

export type CompleteFailureKind =
  // 400 + `missing`: receipts were lost (container recreated between phases) ->
  // re-send exactly those ranges, then complete again.
  | "missing"
  // 409: another completer is running, or the session already completed and the
  // answer is a replay. Both resolve by asking again.
  | "conflict"
  // 400/403/422 without `missing`: the params frozen at init are now rejected
  // (taken access code, unowned token, expiry elapsed) -> resuming is futile.
  | "unrecoverable"
  | "gone"
  | "retryable";

export const classifyCompleteFailure = (error: unknown): CompleteFailureKind => {
  const { status, missing } = asUploadHttpError(error);
  if (Array.isArray(missing) && missing.length > 0) return "missing";
  if (status === 409) return "conflict";
  if (status === 400 || status === 401 || status === 403 || status === 422) {
    return "unrecoverable";
  }
  if (status === 404 || status === 410) return "gone";
  if (status === undefined) return "retryable";
  if (status === 408 || status === 429 || status >= 500) return "retryable";
  return "unrecoverable";
};

// A complete failure the frozen session params cannot fix leaves the session
// rolled back to `active` server-side, so resuming it would fail forever. Such
// a failure drops the resume state; a missing-chunk answer and 409 conflicts
// stay resumable (and "gone" is handled by re-initialising).
export const isUnrecoverableCompleteFailure = (error: unknown): boolean => {
  const kind = classifyCompleteFailure(error);
  return kind === "unrecoverable" || kind === "gone";
};

export const isRetryableUploadError = (error: unknown): boolean =>
  classifyChunkFailure(error) === "retryable";

// ---------------------------------------------------------------------------
// Resume persistence (localStorage) for network-drop / page-reload resume.
// ---------------------------------------------------------------------------

type StoredResume = { uploadId: string; requestId: string };

const readStoredResume = (key: string): StoredResume | null => {
  try {
    const raw = window.localStorage.getItem(key);
    if (!raw) return null;
    // Legacy shape: a plain uploadId string.
    if (raw.startsWith("{")) {
      const parsed = JSON.parse(raw) as Partial<StoredResume>;
      if (typeof parsed.uploadId === "string" && parsed.uploadId) {
        return {
          uploadId: parsed.uploadId,
          requestId:
            typeof parsed.requestId === "string" && parsed.requestId
              ? parsed.requestId
              : ""
        };
      }
      return null;
    }
    const trimmed = raw.trim();
    return trimmed ? { uploadId: trimmed, requestId: "" } : null;
  } catch {
    return null;
  }
};

const writeStoredResume = (key: string, value: StoredResume): void => {
  try {
    window.localStorage.setItem(key, JSON.stringify(value));
  } catch {
    // ignore quota/private-mode failures; resume just won't survive reload
  }
};

export const clearStoredUploadId = (key: string): void => {
  try {
    window.localStorage.removeItem(key);
  } catch {
    // ignore
  }
};

const makeRequestId = (): string => {
  const c = (globalThis as { crypto?: Crypto }).crypto;
  if (c?.randomUUID) return c.randomUUID();
  return `req-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
};

// ---------------------------------------------------------------------------
// Chunked uploader.
// ---------------------------------------------------------------------------

export type UploadStatus =
  | "initializing"
  | "resuming"
  | "uploading"
  | "retrying"
  // The server dropped its receipts (lost/resized container) and asked for the
  // named ranges again: the session survives, progress restarts.
  | "restarting"
  // Every range is written; COMPLETE only inserts the clip row (no merge).
  | "finalizing"
  | "completed"
  | "failed"
  | "cancelled";

export type UploadProgress = {
  uploadId: string | null;
  uploadedBytes: number;
  totalBytes: number;
  uploadedChunks: number;
  totalChunks: number;
  percent: number;
  status: UploadStatus;
  attempt?: number;
  maxAttempts?: number;
  failedIndex?: number;
};

export type ChunkedUploadOptions = {
  file: File;
  filename?: string;
  // Every upload is a file clip: the params (environmentId + expiresAt, plus
  // optional maxDownloads/accessCode/accessToken) travel with INIT, are
  // validated before any chunk consumes storage and are stored on the session.
  // /complete replays them and never accepts them again.
  clip: UploadClipParams;
  // Captcha is verified once at INIT (before any chunk consumes storage).
  captchaToken?: string;
  captchaProvider?: "turnstile" | "recaptcha";
  concurrency?: number;
  chunkTimeoutMs?: number;
  maxRetries?: number;
  completeTimeoutMs?: number;
  signal?: AbortSignal;
  onProgress?: (p: UploadProgress) => void;
};

// Bounded loops: a container reset (409 + missing) may repeat, and COMPLETE may
// have to answer conflicts a few times, but never forever.
const MAX_CHUNK_ROUNDS = 3;
const MAX_COMPLETE_ATTEMPTS = 4;
const SESSION_POLL_ATTEMPTS = 8;

const throwIfAborted = (signal?: AbortSignal): void => {
  if (signal?.aborted) {
    throw new DOMException("Upload cancelled", "AbortError");
  }
};

const makeStatusError = (message: string, status: number): UploadHttpError => {
  const error = new Error(message) as UploadHttpError;
  error.status = status;
  return error;
};

const fetchChunkWithTimeout = async (
  uploadId: string,
  index: number,
  blob: Blob,
  expectedBytes: number,
  timeoutMs: number,
  parentSignal?: AbortSignal
): Promise<Awaited<ReturnType<typeof uploadChunkBytes>>> => {
  const ctrl = new AbortController();
  const onParentAbort = (): void => ctrl.abort();
  if (parentSignal) {
    if (parentSignal.aborted) ctrl.abort();
    else parentSignal.addEventListener("abort", onParentAbort, { once: true });
  }
  const timer = window.setTimeout(() => ctrl.abort(), timeoutMs);
  try {
    return await uploadChunkBytes(uploadId, index, blob, {
      signal: ctrl.signal,
      expectedBytes
    });
  } catch (error) {
    // Distinguish timeout-abort from user-cancel for better messages.
    if (ctrl.signal.aborted && !parentSignal?.aborted) {
      const timeoutErr = new Error(`chunk ${index} timeout`) as UploadHttpError;
      timeoutErr.timeout = true;
      throw timeoutErr;
    }
    throw error;
  } finally {
    window.clearTimeout(timer);
    parentSignal?.removeEventListener("abort", onParentAbort);
  }
};

type RoundOutcome =
  | { kind: "done" }
  | { kind: "reset"; missing: number[] }
  | { kind: "completed" }
  | { kind: "gone"; error: unknown }
  | { kind: "failed"; error: unknown };

export const uploadFileChunked = async (
  options: ChunkedUploadOptions
): Promise<RemoteClip> => {
  const {
    file,
    clip,
    concurrency = 3,
    chunkTimeoutMs = 30_000,
    maxRetries = 3,
    completeTimeoutMs = 90_000,
    signal,
    onProgress
  } = options;
  const environmentId = (clip.environmentId ?? "").trim();
  if (!environmentId) {
    // The server rejects an ownerless upload (a nameless clip without an
    // environment could never be listed again), so fail early & clearly.
    throw new Error("environmentId 缺失，无法创建文件片段");
  }
  if (!Number.isFinite(clip.expiresAt) || clip.expiresAt <= Date.now()) {
    throw new Error("过期时间必须晚于当前时间");
  }
  const filename = (options.filename ?? file.name ?? "").trim() || "uploaded";
  const mimeType = file.type || "application/octet-stream";
  const totalBytes = file.size;
  const resumeKey = buildResumeKey({
    environmentId,
    filename,
    fileSize: totalBytes,
    lastModified: (file as File).lastModified ?? 0,
    accessCode: clip.accessCode,
    accessToken: clip.accessToken
  });

  // -- mutable upload state -------------------------------------------------
  let uploadId: string | null = null;
  let chunkSize = 0;
  let totalChunks = 0;
  let receivedSet = new Set<number>();
  let resumeRound = 0;
  let completeAttempts = 0;
  let cancelled = false;

  const emit = (p: UploadProgress): void => {
    try {
      onProgress?.(p);
    } catch {
      // never let progress callbacks break the upload
    }
  };

  const uploadedBytesFor = (): number => {
    let n = 0;
    for (const idx of receivedSet) {
      const { start, end } = getChunkRange(idx, chunkSize, totalBytes);
      n += Math.max(0, end - start);
    }
    return n;
  };

  const percentFor = (): number =>
    totalBytes === 0 ? 100 : Math.min(100, Math.round((uploadedBytesFor() / totalBytes) * 100));

  const emitProgress = (
    status: UploadStatus,
    extra?: Pick<UploadProgress, "attempt" | "maxAttempts" | "failedIndex">
  ): void => {
    emit({
      uploadId,
      uploadedBytes: uploadedBytesFor(),
      totalBytes,
      uploadedChunks: receivedSet.size,
      totalChunks,
      percent: percentFor(),
      status,
      ...extra
    });
  };

  const delay = (ms: number): Promise<void> =>
    new Promise((resolve) => window.setTimeout(resolve, ms));

  // Rolls a doomed session back: best-effort DELETE (releases the reservation
  // and deletes the pre-allocated file immediately) plus dropping the resume
  // slot so the next attempt re-inits with fresh params.
  const discardResumeState = async (): Promise<void> => {
    if (uploadId) {
      try {
        await abortFileUpload(uploadId);
      } catch {
        // best effort: the TTL sweep reaps it
      }
    }
    clearStoredUploadId(resumeKey);
  };

  const abortUpload = async (): Promise<never> => {
    try {
      if (uploadId) await abortFileUpload(uploadId);
    } catch {
      // ignore: TTL worker will reap it
    }
    clearStoredUploadId(resumeKey);
    emitProgress("cancelled");
    throw new DOMException("Upload cancelled", "AbortError");
  };

  // 409 on INIT = the (environmentId, requestId) idempotency key is still held
  // by a session that is mid-complete; the very same request succeeds once that
  // row reaches a terminal state.
  const initUploadWithConflictRetry = async (
    params: Parameters<typeof initFileUpload>[0],
    attempts: number
  ): Promise<Awaited<ReturnType<typeof initFileUpload>>> => {
    for (let attempt = 1; ; attempt += 1) {
      try {
        return await initFileUpload(params);
      } catch (error) {
        const status = asUploadHttpError(error).status;
        if (status === 409 && attempt <= attempts) {
          await delay(backoffDelayMs(attempt));
          continue;
        }
        throw error;
      }
    }
  };

  // Probe used when a chunk PUT answers 409 without `missing`: the status alone
  // says whether to replay complete, wait, or retry the range.
  const resolveSessionConflict = async (
    id: string
  ): Promise<"completed" | "active" | "gone" | "unknown"> => {
    for (let poll = 0; poll < SESSION_POLL_ATTEMPTS; poll += 1) {
      try {
        const info = await getUploadInfo(id);
        if (info.status === "completed") return "completed";
        if (info.status === "active") return "active";
        if (info.status !== "completing") return "unknown";
      } catch (error) {
        const status = asUploadHttpError(error).status;
        if (status === 404 || status === 410) return "gone";
        return "unknown";
      }
      await delay(Math.min(2000, 500 * (poll + 1)));
    }
    return "unknown";
  };

  const doComplete = async (): Promise<RemoteClip> => {
    const completeCtrl = new AbortController();
    const onParentAbort = (): void => completeCtrl.abort();
    if (signal) {
      if (signal.aborted) completeCtrl.abort();
      else signal.addEventListener("abort", onParentAbort, { once: true });
    }
    const completeTimer = window.setTimeout(() => completeCtrl.abort(), completeTimeoutMs);
    try {
      return await completeUpload(uploadId as string, { signal: completeCtrl.signal });
    } finally {
      window.clearTimeout(completeTimer);
      signal?.removeEventListener("abort", onParentAbort);
    }
  };

  // -- Phase 1: init or resume (server generates uploadId/chunkSize) ---------
  emit({
    uploadId: null,
    uploadedBytes: 0,
    totalBytes,
    uploadedChunks: 0,
    totalChunks: 0,
    percent: 0,
    status: "initializing"
  });

  const stored = readStoredResume(resumeKey);
  let requestId = stored?.requestId ?? "";
  let resumed = false;

  const adoptServerState = (info: UploadInfoResponse): void => {
    uploadId = info.uploadId;
    chunkSize = info.chunkSize;
    totalChunks = info.totalChunks;
    receivedSet = new Set(deriveChunkState(info).received);
  };

  if (stored?.uploadId && !requestId) {
    // Legacy resume state (uploadId only): resume via GET /uploads/{id}. Its
    // progress comes from the DB (receivedChunks/missingChunks), so adopting it
    // needs no chunk-directory scan.
    try {
      const info = await getUploadInfo(stored.uploadId);
      if (
        info.status === "active" &&
        info.fileSize === totalBytes &&
        info.filename === filename &&
        info.expiresAt > Date.now()
      ) {
        adoptServerState(info);
        resumed = true;
      } else if (info.status === "completed") {
        clearStoredUploadId(resumeKey);
      } else {
        clearStoredUploadId(resumeKey);
      }
    } catch {
      clearStoredUploadId(resumeKey);
    }
    throwIfAborted(signal);
  }

  if (!uploadId) {
    if (!requestId) requestId = makeRequestId();
    const init = await initUploadWithConflictRetry(
      {
        filename,
        fileSize: totalBytes,
        mimeType,
        environmentId,
        requestId,
        captchaToken: options.captchaToken,
        captchaProvider: options.captchaProvider,
        // Clip params: validated + persisted server-side at init, replayed by
        // complete (which never accepts them again).
        expiresAt: clip.expiresAt,
        maxDownloads: clip.maxDownloads,
        accessCode: clip.accessCode,
        accessToken: clip.accessToken
      },
      maxRetries
    );
    uploadId = init.uploadId;
    chunkSize = init.chunkSize;
    totalChunks = init.totalChunks;
    if (init.fileSize !== totalBytes) {
      await discardResumeState();
      throw makeStatusError("服务端返回的文件大小与本地文件不一致，请重新选择文件", 400);
    }
    const planError = verifyChunkPlan({ fileSize: totalBytes, chunkSize, totalChunks });
    if (planError) {
      await discardResumeState();
      throw makeStatusError(planError, 400);
    }
    receivedSet = new Set(normalizeIndices(init.receivedChunks, totalChunks));
    writeStoredResume(resumeKey, { uploadId, requestId });
    // A replayed init may report "completed" (the previous run finished but its
    // complete-response never reached us): skip straight to complete, which
    // replays the existing clip instead of creating a duplicate.
    resumed = resumed || receivedSet.size > 0;
  }

  emitProgress(resumed ? "resuming" : "uploading");

  // -- Phase 2: PUT the missing ranges --------------------------------------
  const putChunkWithRetry = async (
    index: number,
    blob: Blob,
    expectedBytes: number,
    setOutcome: (outcome: RoundOutcome) => void,
    hasOutcome: () => boolean
  ): Promise<void> => {
    for (let attempt = 1; ; attempt += 1) {
      throwIfAborted(signal);
      if (hasOutcome()) return;
      try {
        await fetchChunkWithTimeout(
          uploadId as string,
          index,
          blob,
          expectedBytes,
          chunkTimeoutMs,
          signal
        );
        return;
      } catch (error) {
        if (signal?.aborted) {
          cancelled = true;
          return;
        }
        const kind = classifyChunkFailure(error);
        if (kind === "reset") {
          // The container was recreated and every receipt dropped: the answer
          // names the ranges to send again (all of them in that case).
          const missing = normalizeIndices(
            asUploadHttpError(error).missing,
            totalChunks
          );
          setOutcome({ kind: "reset", missing });
          return;
        }
        if (kind === "session") {
          const resolution = await resolveSessionConflict(uploadId as string);
          if (resolution === "completed") {
            setOutcome({ kind: "completed" });
            return;
          }
          if (resolution === "gone") {
            setOutcome({ kind: "gone", error });
            return;
          }
          // "active" = a failed complete rolled the session back (params can
          // still be fixed by retrying), "unknown" = still busy or a flaky
          // probe. Both are worth another PUT inside the retry budget.
          if (attempt > maxRetries) {
            setOutcome({ kind: "failed", error });
            return;
          }
          emitProgress("retrying", {
            attempt,
            maxAttempts: maxRetries + 1,
            failedIndex: index
          });
          await delay(backoffDelayMs(attempt));
          continue;
        }
        if (kind === "gone") {
          setOutcome({ kind: "gone", error });
          return;
        }
        if (kind === "fatal") {
          setOutcome({ kind: "failed", error });
          return;
        }
        if (attempt > maxRetries) {
          setOutcome({ kind: "failed", error });
          return;
        }
        emitProgress("retrying", {
          attempt,
          maxAttempts: maxRetries + 1,
          failedIndex: index
        });
        await delay(backoffDelayMs(attempt));
      }
    }
  };

  const runChunkRound = async (indices: number[]): Promise<RoundOutcome> => {
    // Shared queue (single-threaded JS: shift() is atomic enough).
    const queue: number[] = [...indices];
    let outcome: RoundOutcome | null = null;
    const setOutcome = (next: RoundOutcome): void => {
      if (!outcome) outcome = next;
    };

    const worker = async (): Promise<void> => {
      while (queue.length > 0 && !outcome) {
        if (signal?.aborted) {
          cancelled = true;
          return;
        }
        const index = queue.shift();
        if (index === undefined) return;
        const { start, end } = getChunkRange(index, chunkSize, totalBytes);
        // Length is fixed by the plan: exactly chunkSize bytes, except the last
        // chunk which carries the remainder.
        const expected = expectedChunkSize(index, chunkSize, totalBytes);
        const blob = file.slice(start, end, mimeType);
        const payloadError = verifyChunkPayload(index, blob.size, chunkSize, totalBytes);
        if (payloadError) {
          setOutcome({ kind: "failed", error: makeStatusError(payloadError, 400) });
          return;
        }
        await putChunkWithRetry(index, blob, expected, setOutcome, () => outcome !== null);
        if (outcome || signal?.aborted) return;
        receivedSet.add(index);
        emitProgress("uploading");
      }
    };

    const workers: Promise<void>[] = [];
    const n = Math.max(1, Math.min(concurrency, queue.length));
    for (let i = 0; i < n; i += 1) workers.push(worker());
    await Promise.all(workers);
    return outcome ?? { kind: "done" };
  };

  // -- Phase 3: complete (insert the clip against the pre-allocated file) ----
  let uploadFinished = totalChunks === 0;
  for (;;) {
    throwIfAborted(signal);
    if (cancelled || signal?.aborted) {
      return abortUpload();
    }

    if (!uploadFinished) {
      const missing = getMissingIndices(totalChunks, [...receivedSet]);
      if (missing.length === 0) {
        uploadFinished = true;
      } else {
        emitProgress(resumed && resumeRound === 0 ? "resuming" : "uploading");
        const outcome = await runChunkRound(missing);
        if (outcome.kind === "reset") {
          resumeRound += 1;
          if (resumeRound > MAX_CHUNK_ROUNDS) {
            await discardResumeState();
            const error = makeStatusError("服务端连续重置上传进度，请重新上传", 409);
            emitProgress("failed");
            throw error;
          }
          // The server's list is authoritative: drop exactly those receipts and
          // re-send them in the next round.
          for (const idx of outcome.missing) receivedSet.delete(idx);
          emitProgress("restarting");
          continue;
        }
        if (outcome.kind === "completed") {
          // Finished by another attempt/device: complete replays the clip.
          uploadFinished = true;
        } else if (outcome.kind === "gone") {
          clearStoredUploadId(resumeKey);
          emitProgress("failed");
          throw outcome.error;
        } else if (outcome.kind === "failed") {
          emitProgress("failed");
          throw outcome.error;
        }
        continue;
      }
    }

    // Every range is written and fsynced; COMPLETE has no merge step, it only
    // verifies the receipts and inserts the clip row.
    emitProgress("finalizing");
    try {
      const clip = await doComplete();
      clearStoredUploadId(resumeKey);
      emitProgress("completed");
      return clip;
    } catch (error) {
      if (signal?.aborted || cancelled) {
        return abortUpload();
      }
      completeAttempts += 1;
      const kind = classifyCompleteFailure(error);
      const missing = asUploadHttpError(error).missing;
      if (Array.isArray(missing) && missing.length > 0 && completeAttempts <= MAX_COMPLETE_ATTEMPTS) {
        // A range lost its receipt between phases: re-upload exactly those and
        // complete again.
        for (const idx of missing) receivedSet.delete(idx);
        uploadFinished = false;
        continue;
      }
      if ((kind === "conflict" || kind === "retryable") && completeAttempts <= MAX_COMPLETE_ATTEMPTS) {
        // 409 = another completer is running or the session already completed
        // (the retry returns the stored clip); 5xx/network = transient.
        await delay(backoffDelayMs(completeAttempts));
        continue;
      }
      if (kind === "unrecoverable" || kind === "gone") {
        await discardResumeState();
      }
      if (kind === "gone") {
        clearStoredUploadId(resumeKey);
      }
      emitProgress("failed");
      throw error;
    }
  }
};
