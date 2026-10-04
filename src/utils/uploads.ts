import {
  abortFileUpload,
  completeUpload,
  getUploadInfo,
  initFileUpload,
  uploadChunkBytes,
  type UploadClipParams,
  type UploadInfoResponse
} from "./api";
import type { RemoteClip } from "../store/useClipboardStore";

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

// A complete that fails for a reason the frozen session params cannot fix (a
// short code taken by a parallel upload, the expiry elapsing while the bytes
// were in flight, a token re-registered elsewhere) leaves the session rolled
// back to `active` server-side. Resuming that same session would fail forever,
// so such a failure drops the resume state -- while a missing chunk (400 with a
// `missing` list) and transient/network errors stay resumable.
export const isUnrecoverableCompleteFailure = (error: unknown): boolean => {
  const status = (error as Error & { status?: number }).status;
  const missing = (error as Error & { missing?: number[] }).missing;
  if (Array.isArray(missing) && missing.length > 0) return false;
  return status === 400 || status === 409 || status === 410;
};

// Resume entries now persist both the server uploadId and the client
// requestId so that a re-init (page reload, network dropout) replays the SAME
// session server-side instead of leaking a duplicate session dir.
type StoredResume = { uploadId: string; requestId: string };

const makeRequestId = (): string => {
  const c = (globalThis as { crypto?: Crypto }).crypto;
  if (c?.randomUUID) return c.randomUUID();
  return `req-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
};

export const isRetryableUploadError = (error: unknown): boolean => {
  if (error instanceof DOMException && error.name === "AbortError") return false;
  if (error instanceof Error) {
    const status = (error as Error & { status?: number }).status;
    // 4xx (except 408/409/429) are not retryable: bad request, auth, gone.
    // 404/410 mean the session is gone -> must re-init, not retry the chunk.
    if (status === 404 || status === 410) return false;
    if (status === 400 || status === 409 || status === 413 || status === 422) return false;
    if (status === 408 || status === 429 || (status !== undefined && status >= 500)) return true;
    // Network failures (TypeError from fetch) carry no status -> retryable.
    if (status === undefined) {
      const msg = error.message.toLowerCase();
      if (msg.includes("abort")) return false;
      if (msg.includes("cancel")) return false;
      return true;
    }
    return false;
  }
  return true;
};

// ---------------------------------------------------------------------------
// Resume persistence (localStorage) for network-drop / page-reload resume.
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Chunked uploader.
// ---------------------------------------------------------------------------

export type UploadStatus =
  | "initializing"
  | "resuming"
  | "uploading"
  | "retrying"
  | "assembling"
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

const throwIfAborted = (signal?: AbortSignal): void => {
  if (signal?.aborted) {
    const err = new DOMException("Upload cancelled", "AbortError");
    throw err;
  }
};

const fetchChunkWithTimeout = async (
  uploadId: string,
  index: number,
  blob: Blob,
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
    return await uploadChunkBytes(uploadId, index, blob, { signal: ctrl.signal });
  } catch (error) {
    // Distinguish timeout-abort from user-cancel for better messages.
    if (ctrl.signal.aborted && !parentSignal?.aborted) {
      const timeoutErr = new Error(`chunk ${index} timeout`) as Error & {
        status?: number;
        timeout?: boolean;
      };
      timeoutErr.timeout = true;
      throw timeoutErr;
    }
    throw error;
  } finally {
    window.clearTimeout(timer);
    parentSignal?.removeEventListener("abort", onParentAbort);
  }
};

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

  const emit = (p: UploadProgress): void => {
    try {
      onProgress?.(p);
    } catch {
      // never let progress callbacks break the upload
    }
  };

  // Rolls a doomed session back: best-effort DELETE (releases the quota
  // reservation and the chunk dir immediately) plus dropping the resume slot so
  // the next attempt re-inits with fresh params.
  const discardResumeState = async (): Promise<void> => {
    if (uploadId) {
      try {
        await abortFileUpload(uploadId);
      } catch {
        // 404/410: already gone, or the TTL worker will reap it.
      }
    }
    clearStoredUploadId(resumeKey);
  };

  throwIfAborted(signal);

  // -- Phase 1: init or resume (server generates uploadId/chunkSize) ---------
  let uploadId: string | null = null;
  let chunkSize = 0;
  let totalChunks = 0;
  let received: number[] = [];
  let resumed = false;

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
  // Idempotent init is keyed by (environmentId, requestId): reusing the key
  // replays the same server session (network retry after a lost init
  // response). A brand-new file gets a new key via makeRequestId().
  if (stored?.uploadId && !requestId) {
    // Legacy resume state (uploadId only): resume via GET /uploads/{id}.
    try {
      const info: UploadInfoResponse = await getUploadInfo(stored.uploadId);
      if (
        info.status === "active" &&
        info.fileSize === totalBytes &&
        info.filename === filename &&
        info.expiresAt > Date.now()
      ) {
        uploadId = info.uploadId;
        chunkSize = info.chunkSize;
        totalChunks = info.totalChunks;
        received = [...info.receivedChunks].sort((a, b) => a - b);
        resumed = true;
      } else if (info.status === "completed") {
        clearStoredUploadId(resumeKey);
      }
    } catch {
      clearStoredUploadId(resumeKey);
    }
    throwIfAborted(signal);
  }

  if (!uploadId) {
    if (!requestId) requestId = makeRequestId();
    const init = await initFileUpload({
      filename,
      fileSize: totalBytes,
      mimeType,
      environmentId,
      requestId,
      captchaToken: options.captchaToken,
      captchaProvider: options.captchaProvider,
      // Clip params: validated + persisted server-side at init.
      expiresAt: clip.expiresAt,
      maxDownloads: clip.maxDownloads,
      accessCode: clip.accessCode,
      accessToken: clip.accessToken
    });
    uploadId = init.uploadId;
    chunkSize = init.chunkSize;
    totalChunks = init.totalChunks;
    // A replayed init may report "completed" — e.g. the previous run finished
    // but its complete-response never reached us. Skip uploads and replay
    // complete directly so the stored clip is returned (no duplicates).
    if (init.status === "completed") {
      received = [];
      writeStoredResume(resumeKey, { uploadId, requestId });
      // fall through to complete phase with an empty-queue fast path
      emit({
        uploadId,
        uploadedBytes: totalBytes,
        totalBytes,
        uploadedChunks: totalChunks,
        totalChunks,
        percent: 100,
        status: "resuming"
      });
      return completeWithReplay();
    }
    received = (init.receivedChunks ?? []).slice().sort((a, b) => a - b);
    resumed = resumed || received.length > 0 || init.status === "active" && requestId === stored?.requestId && stored?.uploadId === uploadId;
    writeStoredResume(resumeKey, { uploadId, requestId });
  }

  // doComplete performs a single /complete call with timeout + cancel
  // handling. It does not read the chunk-progress state, so it can also run
  // from the "init replayed a completed session" early-exit path.
  const doComplete = async (): Promise<RemoteClip> => {
    const completeCtrl = new AbortController();
    const onParentAbort = (): void => completeCtrl.abort();
    if (signal) {
      if (signal.aborted) completeCtrl.abort();
      else signal.addEventListener("abort", onParentAbort, { once: true });
    }
    const completeTimer = window.setTimeout(() => completeCtrl.abort(), completeTimeoutMs);
    try {
      return await completeUpload(uploadId as string, {
        signal: completeCtrl.signal
      });
    } finally {
      window.clearTimeout(completeTimer);
      signal?.removeEventListener("abort", onParentAbort);
    }
  };

  function completeWithReplay(): Promise<RemoteClip> {
    return (async () => {
      try {
        const clipResult = await doComplete();
        clearStoredUploadId(resumeKey);
        emit({
          uploadId, uploadedBytes: totalBytes, totalBytes,
          uploadedChunks: totalChunks, totalChunks, percent: 100, status: "completed"
        });
        return clipResult;
      } catch (error) {
        if (signal?.aborted) {
          emit({
            uploadId, uploadedBytes: totalBytes, totalBytes,
            uploadedChunks: totalChunks, totalChunks, percent: 100, status: "cancelled"
          });
          throw new DOMException("Upload cancelled", "AbortError");
        }
        if (isUnrecoverableCompleteFailure(error)) {
          await discardResumeState();
        }
        emit({
          uploadId, uploadedBytes: totalBytes, totalBytes,
          uploadedChunks: totalChunks, totalChunks, percent: 100, status: "failed"
        });
        throw error;
      }
    })();
  }

  async function performCompletePhase(): Promise<RemoteClip> {
    try {
      const clipResult = await doComplete();
      clearStoredUploadId(resumeKey);
      emit({
        uploadId, uploadedBytes: totalBytes, totalBytes,
        uploadedChunks: totalChunks, totalChunks, percent: 100, status: "completed"
      });
      return clipResult;
    } catch (error) {
      if (signal?.aborted) {
        emit({
          uploadId,
          uploadedBytes: uploadedBytesFor(),
          totalBytes,
          uploadedChunks,
          totalChunks,
          percent: totalBytes === 0 ? 100 : Math.round((uploadedBytesFor() / totalBytes) * 100),
          status: "cancelled"
        });
        throw new DOMException("Upload cancelled", "AbortError");
      }
      // Missing-chunk errors from complete carry .missing: prune local progress
      // so the next attempt re-uploads exactly those.
      const missing = (error as Error & { missing?: number[] }).missing;
      if (Array.isArray(missing) && missing.length > 0) {
        for (const idx of missing) receivedSet.delete(idx);
        uploadedChunks = receivedSet.size;
      } else if (isUnrecoverableCompleteFailure(error)) {
        // Rolled-back session (conflict / expired params): a resume can never
        // succeed, so clear it and let the user retry with fresh params.
        await discardResumeState();
      }
      emit({
        uploadId,
        uploadedBytes: uploadedBytesFor(),
        totalBytes,
        uploadedChunks,
        totalChunks,
        percent: totalBytes === 0 ? 100 : Math.round((uploadedBytesFor() / totalBytes) * 100),
        status: "failed"
      });
      throw error;
    }
  }

  const receivedSet = new Set(received);
  let uploadedChunks = receivedSet.size;
  const uploadedBytesFor = (): number => {
    let n = 0;
    for (const idx of receivedSet) {
      const { start, end } = getChunkRange(idx, chunkSize, totalBytes);
      n += Math.max(0, end - start);
    }
    return n;
  };

  emit({
    uploadId,
    uploadedBytes: uploadedBytesFor(),
    totalBytes,
    uploadedChunks,
    totalChunks,
    percent: totalBytes === 0 ? 100 : Math.round((uploadedBytesFor() / totalBytes) * 100),
    status: resumed ? "resuming" : "uploading"
  });

  // -- Phase 2: upload missing chunks with bounded concurrency -------------
  const missing = getMissingIndices(totalChunks, [...receivedSet]);
  // Shared queue (single-threaded JS: shift() is atomic enough).
  const queue: number[] = [...missing];
  let failed: unknown = null;
  let cancelled = false;

  const worker = async (): Promise<void> => {
    while (queue.length > 0) {
      if (failed || cancelled) return;
      throwIfAborted(signal);
      const index = queue.shift();
      if (index === undefined) return;
      const { start, end } = getChunkRange(index, chunkSize, totalBytes);
      const blob = file.slice(start, end, mimeType);
      let attempt = 0;
      for (;;) {
        attempt += 1;
        throwIfAborted(signal);
        try {
          if (attempt > 1) {
            emit({
              uploadId,
              uploadedBytes: uploadedBytesFor(),
              totalBytes,
              uploadedChunks,
              totalChunks,
              percent: totalBytes === 0 ? 100 : Math.round((uploadedBytesFor() / totalBytes) * 100),
              status: "retrying",
              attempt,
              maxAttempts: maxRetries + 1,
              failedIndex: index
            });
          }
          await fetchChunkWithTimeout(uploadId as string, index, blob, chunkTimeoutMs, signal);
          receivedSet.add(index);
          uploadedChunks = receivedSet.size;
          emit({
            uploadId,
            uploadedBytes: uploadedBytesFor(),
            totalBytes,
            uploadedChunks,
            totalChunks,
            percent: totalBytes === 0 ? 100 : Math.round((uploadedBytesFor() / totalBytes) * 100),
            status: "uploading"
          });
          break; // next chunk
        } catch (error) {
          if (signal?.aborted) {
            cancelled = true;
            failed = error;
            return;
          }
          const retryable = isRetryableUploadError(error);
          // Session gone (404/410): stop everything, caller will re-init on retry.
          const status = (error as Error & { status?: number }).status;
          if (status === 404 || status === 410) {
            clearStoredUploadId(resumeKey);
            failed = error;
            return;
          }
          if (!retryable || attempt > maxRetries) {
            failed = error;
            return;
          }
          await new Promise((r) => window.setTimeout(r, backoffDelayMs(attempt)));
        }
      }
    }
  };

  if (queue.length > 0) {
    const workers: Promise<void>[] = [];
    const n = Math.max(1, Math.min(concurrency, queue.length));
    for (let i = 0; i < n; i += 1) workers.push(worker());
    await Promise.all(workers);
  }

  if (signal?.aborted || cancelled) {
    // User cancel: best-effort server cleanup (DELETE) + drop resume state.
    try {
      if (uploadId) await abortFileUpload(uploadId);
    } catch {
      // ignore: TTL worker will reap it
    }
    clearStoredUploadId(resumeKey);
    emit({
      uploadId,
      uploadedBytes: uploadedBytesFor(),
      totalBytes,
      uploadedChunks,
      totalChunks,
      percent: totalBytes === 0 ? 100 : Math.round((uploadedBytesFor() / totalBytes) * 100),
      status: "cancelled"
    });
    throw new DOMException("Upload cancelled", "AbortError");
  }

  if (failed) {
    emit({
      uploadId,
      uploadedBytes: uploadedBytesFor(),
      totalBytes,
      uploadedChunks,
      totalChunks,
      percent: totalBytes === 0 ? 100 : Math.round((uploadedBytesFor() / totalBytes) * 100),
      status: "failed"
    });
    // Keep resume state (do NOT delete) so a manual retry can continue.
    throw failed;
  }

  // -- Phase 3: complete (assemble + insert the file clip, no params) --------
  emit({
    uploadId,
    uploadedBytes: totalBytes,
    totalBytes,
    uploadedChunks: totalChunks,
    totalChunks,
    percent: 100,
    status: "assembling"
  });
  return performCompletePhase();
};
