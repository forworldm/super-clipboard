import { ClipType, RemoteClip } from "../store/useClipboardStore";

const API_BASE = "/api";

type ApiFilePayload = {
  name: string;
  size: number;
  type: string;
  downloadUrl: string;
} | null;

type ApiClip = {
  id: string;
  type: ClipType;
  createdAt: number;
  expiresAt: number;
  maxDownloads: number;
  downloadCount: number;
  accessCode?: string | null;
  accessToken?: string | null;
  payload: {
    text?: string | null;
    file?: ApiFilePayload;
  };
  directUrl?: string | null;
};

type CreateClipPayload = {
  type: ClipType;
  expiresAt: number;
  maxDownloads: number;
  environmentId: string;
  accessCode?: string;
  accessToken?: string;
  captchaToken?: string;
  captchaProvider?: "turnstile" | "recaptcha";
  payload: {
    text?: string;
    file?: {
      name: string;
      size: number;
      type: string;
      dataUrl: string;
    };
  };
};

const request = async <T>(input: RequestInfo, init?: RequestInit): Promise<T> => {
  const response = await fetch(input, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      ...init?.headers
    }
  });

  if (!response.ok) {
    let detail: unknown = null;
    try {
      detail = await response.json();
    } catch (error) {
      detail = await response.text();
    }
    const payload = typeof detail === "string" ? { message: detail } : (detail as Record<string, unknown>);
    const data = payload as { detail?: unknown; error?: unknown; message?: unknown };
    const message =
      typeof data.detail === "string"
        ? data.detail
        : typeof data.error === "string"
        ? data.error
        : typeof data.message === "string"
        ? data.message
        : "请求失败";
    const error = new Error(message);
    throw error;
  }

  return (await response.json()) as T;
};

const mapClip = (clip: ApiClip): RemoteClip => ({
  id: clip.id,
  type: clip.type,
  createdAt: clip.createdAt,
  expiresAt: clip.expiresAt,
  maxDownloads: clip.maxDownloads,
  downloadCount: clip.downloadCount,
  accessCode: clip.accessCode ?? undefined,
  accessToken: clip.accessToken ?? undefined,
  payload: {
    text: clip.payload?.text ?? null,
    file: clip.payload?.file ?? null
  },
  directUrl: clip.directUrl ?? undefined
});

export const listRemoteClips = async (environmentId: string): Promise<RemoteClip[]> => {
  const data = await request<{ items: ApiClip[] }>(
    `${API_BASE}/clips?environmentId=${encodeURIComponent(environmentId)}`
  );
  return data.items.map(mapClip);
};

export const createRemoteClip = async (
  payload: CreateClipPayload
): Promise<RemoteClip> => {
  const data = await request<ApiClip>(`${API_BASE}/clips`, {
    method: "POST",
    body: JSON.stringify(payload)
  });
  return mapClip(data);
};

type RegisterTokenResponse = {
  token: string;
  environmentId: string;
  updatedAt: number;
  lastUsedAt?: number | null;
  expiresAt: number;
};

export type AppConfig = {
  captchaProvider?: "turnstile" | "recaptcha";
  captchaSiteKey?: string;
  maxFileSizeBytes?: number;
  uploadChunkSizeBytes?: number;
  uploadSessionTTLSeconds?: number;
};

// ---------------------------------------------------------------------------
// Chunked uploads (server-generated uploadId/chunkSize, raw chunk bytes).
// ---------------------------------------------------------------------------

export type UploadInitResponse = {
  uploadId: string;
  chunkSize: number;
  totalChunks: number;
  fileSize: number;
  filename: string;
  mimeType: string;
  expiresAt: number;
  // "active" for fresh uploads; "completed" when an idempotent init replay
  // lands on an already-merged session (client should call complete as-is,
  // which replays the previously created clip instead of re-creating one).
  status: string;
  // Populated on idempotent replays (already-received chunk indices);
  // empty array for brand-new sessions.
  receivedChunks: number[];
};

export type UploadInfoResponse = {
  uploadId: string;
  filename: string;
  fileSize: number;
  mimeType: string;
  chunkSize: number;
  totalChunks: number;
  receivedChunks: number[];
  receivedCount: number;
  missingChunks: number[];
  status: string;
  createdAt: number;
  updatedAt: number;
  expiresAt: number;
};

export type UploadChunkResponse = {
  uploadId: string;
  index: number;
  receivedCount: number;
  totalChunks: number;
  receivedChunks: number[];
  complete: boolean;
};

// Every chunked upload becomes a file clip, so these params are part of the
// INIT request: they are validated and persisted server-side before a single
// chunk is stored, and /complete replays the stored copy (it never accepts
// them again). A clip without accessCode/accessToken is a nameless clip that
// only its environment owner can see.
export type UploadClipParams = {
  // Required: the clip's owner is the only way to list a nameless clip.
  environmentId: string;
  // Required: clips.expiresAt must be in the future.
  expiresAt: number;
  maxDownloads?: number;
  accessCode?: string;
  accessToken?: string;
};

export type UploadInitParams = {
  filename: string;
  fileSize: number;
  mimeType?: string;
  environmentId: string;
  // Client-generated idempotency key: safe init retries return the same
  // session (replay) instead of creating a duplicate session on disk.
  requestId?: string;
  captchaToken?: string;
  captchaProvider?: "turnstile" | "recaptcha";
  // Clip params, validated and persisted at init.
  expiresAt: number;
  maxDownloads?: number;
  accessCode?: string;
  accessToken?: string;
};

const requestRaw = async <T>(
  input: RequestInfo,
  init?: RequestInit
): Promise<T> => {
  const response = await fetch(input, init);
  if (!response.ok) {
    let detail: unknown = null;
    try {
      detail = await response.json();
    } catch {
      try {
        detail = await response.text();
      } catch {
        detail = null;
      }
    }
    const payload =
      typeof detail === "string" ? { message: detail } : (detail as Record<string, unknown> | null);
    const data = (payload ?? {}) as {
      detail?: unknown;
      Detail?: unknown;
      error?: unknown;
      message?: unknown;
      missing?: unknown;
    };
    const rawDetail = data.detail ?? data.Detail ?? data.error ?? data.message;
    const message =
      typeof rawDetail === "string"
        ? rawDetail
        : Array.isArray(rawDetail)
        ? JSON.stringify(rawDetail)
        : "请求失败";
    const error = new Error(message) as Error & {
      status?: number;
      missing?: number[];
    };
    error.status = response.status;
    if (Array.isArray(data.missing)) {
      error.missing = (data.missing as unknown[]).filter(
        (v): v is number => typeof v === "number"
      );
    }
    throw error;
  }
  return (await response.json()) as T;
};

export const initFileUpload = async (
  params: UploadInitParams
): Promise<UploadInitResponse> => {
  // environmentId + expiresAt are mandatory server-side: every upload becomes a
  // file clip and a clip needs an owner and an expiry.
  const body: Record<string, unknown> = {
    filename: params.filename,
    fileSize: params.fileSize,
    mimeType: params.mimeType ?? "application/octet-stream",
    environmentId: params.environmentId,
    expiresAt: params.expiresAt
  };
  if (params.requestId) body.requestId = params.requestId;
  if (params.captchaToken) body.captchaToken = params.captchaToken;
  if (params.captchaProvider) body.captchaProvider = params.captchaProvider;
  if (params.maxDownloads != null) body.maxDownloads = params.maxDownloads;
  if (params.accessCode) body.accessCode = params.accessCode;
  if (params.accessToken) body.accessToken = params.accessToken;
  return request<UploadInitResponse>(`${API_BASE}/uploads/init`, {
    method: "POST",
    body: JSON.stringify(body)
  });
};

export const getUploadInfo = async (
  uploadId: string
): Promise<UploadInfoResponse> => {
  return request<UploadInfoResponse>(
    `${API_BASE}/uploads/${encodeURIComponent(uploadId)}`
  );
};

export const uploadChunkBytes = async (
  uploadId: string,
  index: number,
  blob: Blob,
  opts?: { signal?: AbortSignal }
): Promise<UploadChunkResponse> => {
  return requestRaw<UploadChunkResponse>(
    `${API_BASE}/uploads/${encodeURIComponent(uploadId)}/chunks/${index}`,
    {
      method: "PUT",
      headers: { "Content-Type": "application/octet-stream" },
      body: blob,
      signal: opts?.signal
    }
  );
};

// completeUpload assembles the chunks and inserts the file clip, using the clip
// params frozen at init. It carries NO body: there is no file-only mode, and
// re-sending params here would be ignored anyway.
export const completeUpload = async (
  uploadId: string,
  opts?: { signal?: AbortSignal }
): Promise<RemoteClip> => {
  const data = await requestRaw<ApiClip>(
    `${API_BASE}/uploads/${encodeURIComponent(uploadId)}/complete`,
    {
      method: "POST",
      signal: opts?.signal
    }
  );
  return mapClip(data);
};

export const abortFileUpload = async (uploadId: string): Promise<void> => {
  await request(`${API_BASE}/uploads/${encodeURIComponent(uploadId)}`, {
    method: "DELETE"
  });
};

export const registerPersistentToken = async (
  token: string,
  environmentId?: string
): Promise<RegisterTokenResponse> => {
  const body: Record<string, string> = { token };
  if (environmentId) {
    body.environmentId = environmentId;
  }
  return request<RegisterTokenResponse>(`${API_BASE}/tokens/register`, {
    method: "POST",
    body: JSON.stringify(body)
  });
};

export const deleteRemoteClip = async (clipId: string, environmentId: string): Promise<void> => {
  await request(
    `${API_BASE}/clips/${clipId}?environmentId=${encodeURIComponent(environmentId)}`,
    {
      method: "DELETE"
    }
  );
};

export const fetchRemoteClip = async (
  clipId: string,
  environmentId: string
): Promise<RemoteClip> => {
  const data = await request<ApiClip>(
    `${API_BASE}/clips/${clipId}?environmentId=${encodeURIComponent(environmentId)}`
  );
  return mapClip(data);
};

export const incrementRemoteClip = async (
  clipId: string,
  environmentId: string
): Promise<{ clip: RemoteClip; removed: boolean }> => {
  const data = await request<{ clip: ApiClip; removed: boolean }>(
    `${API_BASE}/clips/${clipId}/download?environmentId=${encodeURIComponent(environmentId)}`,
    {
      method: "POST"
    }
  );
  return {
    clip: mapClip(data.clip),
    removed: data.removed
  };
};

export const fetchAppConfig = async (): Promise<AppConfig> => {
  return request<AppConfig>(`${API_BASE}/config`);
};
