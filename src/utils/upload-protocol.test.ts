import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { UploadInfoResponse } from "./api";
import {
  buildResumeKey,
  calcTotalChunks,
  classifyChunkFailure,
  classifyCompleteFailure,
  clearStoredUploadId,
  deriveChunkState,
  expectedChunkSize,
  getChunkOffset,
  getMissingIndices,
  isUnrecoverableCompleteFailure,
  normalizeIndices,
  uploadFileChunked,
  verifyChunkPayload,
  verifyChunkPlan,
  type UploadProgress
} from "./uploads";

// ---------------------------------------------------------------------------
// The backend no longer stores one file per chunk and no longer merges: INIT
// pre-allocates ONE file (truncate(fileSize)) and every PUT writes its bytes
// into the range [index * chunkSize, +expected), with upload_chunks as the only
// progress record. These tests pin the client half of that contract.
// ---------------------------------------------------------------------------

const makeStorage = (): Storage => {
  const map = new Map<string, string>();
  return {
    get length() {
      return map.size;
    },
    clear: () => map.clear(),
    getItem: (key: string) => map.get(key) ?? null,
    key: (index: number) => [...map.keys()][index] ?? null,
    removeItem: (key: string) => void map.delete(key),
    setItem: (key: string, value: string) => void map.set(key, value)
  } as Storage;
};

let storage: Storage;

beforeEach(() => {
  storage = makeStorage();
  vi.stubGlobal("window", {
    setTimeout: (callback: (...args: unknown[]) => void, ms?: number) =>
      globalThis.setTimeout(callback, ms),
    clearTimeout: (id: unknown) => globalThis.clearTimeout(id as number),
    localStorage: storage
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// ---------------------------------------------------------------------------
// Pure protocol helpers
// ---------------------------------------------------------------------------

describe("chunk plan (fileSize + chunkSize are known at init)", () => {
  it("derives total chunks and range sizes, last chunk carrying the remainder", () => {
    expect(calcTotalChunks(10, 4)).toBe(3);
    expect(calcTotalChunks(8, 4)).toBe(2);
    expect(calcTotalChunks(0, 4)).toBe(0);

    expect(getChunkOffset(0, 4)).toBe(0);
    expect(getChunkOffset(2, 4)).toBe(8);

    expect(expectedChunkSize(0, 4, 10)).toBe(4);
    expect(expectedChunkSize(1, 4, 10)).toBe(4);
    expect(expectedChunkSize(2, 4, 10)).toBe(2);
    // Exactly-divisible sizes keep the last chunk full.
    expect(expectedChunkSize(1, 4, 8)).toBe(4);
  });

  it("verifies a server plan against the local file", () => {
    expect(verifyChunkPlan({ fileSize: 10, chunkSize: 4, totalChunks: 3 })).toBeNull();
    expect(verifyChunkPlan({ fileSize: 0, chunkSize: 4, totalChunks: 0 })).toBeNull();
    expect(verifyChunkPlan({ fileSize: 10, chunkSize: 4, totalChunks: 4 })).toMatch(
      /分片数量与文件大小不一致/
    );
    expect(verifyChunkPlan({ fileSize: 10, chunkSize: 0, totalChunks: 3 })).toMatch(
      /分片大小无效/
    );
    expect(verifyChunkPlan({ fileSize: 0, chunkSize: 4, totalChunks: 1 })).toMatch(
      /分片计划与文件大小不一致/
    );
  });

  it("mirrors the server size contract locally (same wording as the 400 answer)", () => {
    expect(verifyChunkPayload(0, 4, 4, 10)).toBeNull();
    expect(verifyChunkPayload(2, 2, 4, 10)).toBeNull();
    expect(verifyChunkPayload(0, 3, 4, 10)).toBe(
      "分片大小不匹配：第 0 块应为 4 字节，实际 3 字节"
    );
    expect(verifyChunkPayload(2, 4, 4, 10)).toBe(
      "分片大小不匹配：第 2 块应为 2 字节，实际 4 字节"
    );
  });

  it("recomputes the missing set from server-reported progress", () => {
    expect(getMissingIndices(4, [0, 2])).toEqual([1, 3]);
    expect(getMissingIndices(0, [])).toEqual([]);
  });

  it("normalises (dedupes, sorts, bounds) chunk index lists", () => {
    expect(normalizeIndices([2, 0, 2, -1, 1.5, "x"], 3)).toEqual([0, 2]);
    expect(normalizeIndices([3, 4], 3)).toEqual([]);
    expect(normalizeIndices(undefined)).toEqual([]);
  });

  it("projects GET /uploads/{id} into received/missing (DB is the source of truth)", () => {
    const info = {
      uploadId: "u1",
      filename: "a.bin",
      fileSize: 10,
      mimeType: "application/octet-stream",
      chunkSize: 4,
      totalChunks: 3,
      receivedChunks: [0, 2],
      receivedCount: 2,
      missingChunks: [1],
      status: "active",
      createdAt: 1,
      updatedAt: 2,
      expiresAt: 3
    } satisfies UploadInfoResponse;
    expect(deriveChunkState(info)).toEqual({ received: [0, 2], missing: [1] });

    // A replay answer may omit missingChunks: recompute instead of trusting [].
    expect(
      deriveChunkState({ ...info, missingChunks: [] }).missing
    ).toEqual([1]);
    expect(
      deriveChunkState({ ...info, receivedChunks: [0, 1, 2], missingChunks: [] }).missing
    ).toEqual([]);
  });

  it("keys the resume slot by access mode (params are frozen at init)", () => {
    const base = {
      environmentId: "env",
      filename: "a.bin",
      fileSize: 10,
      lastModified: 1
    };
    expect(buildResumeKey(base)).toBe(buildResumeKey({ ...base }));
    expect(buildResumeKey({ ...base, accessCode: "12345" })).not.toBe(
      buildResumeKey({ ...base, accessToken: "abcdefg" })
    );
  });
});

describe("failure classification", () => {
  const httpError = (status?: number, missing?: number[]): Error => {
    const error = new Error("boom") as Error & { status?: number; missing?: number[] };
    if (status !== undefined) error.status = status;
    if (missing) error.missing = missing;
    return error;
  };

  it("classifies chunk PUT answers", () => {
    expect(classifyChunkFailure(httpError(409, [0, 1]))).toBe("reset");
    expect(classifyChunkFailure(httpError(409))).toBe("session");
    expect(classifyChunkFailure(httpError(404))).toBe("gone");
    expect(classifyChunkFailure(httpError(410))).toBe("gone");
    expect(classifyChunkFailure(httpError(400))).toBe("fatal");
    expect(classifyChunkFailure(httpError(413))).toBe("fatal");
    expect(classifyChunkFailure(httpError(422))).toBe("fatal");
    expect(classifyChunkFailure(httpError(429))).toBe("retryable");
    expect(classifyChunkFailure(httpError(500))).toBe("retryable");
    // Typed storage guards (quota / session cap / disk watermark) answer 507.
    expect(classifyChunkFailure(httpError(507))).toBe("retryable");
    expect(classifyChunkFailure(new TypeError("Failed to fetch"))).toBe("retryable");
  });

  it("classifies complete answers (no merge step ever fails here)", () => {
    expect(classifyCompleteFailure(httpError(400, [2]))).toBe("missing");
    expect(classifyCompleteFailure(httpError(409))).toBe("conflict");
    expect(classifyCompleteFailure(httpError(400))).toBe("unrecoverable");
    expect(classifyCompleteFailure(httpError(410))).toBe("gone");
    expect(classifyCompleteFailure(httpError(507))).toBe("retryable");

    expect(isUnrecoverableCompleteFailure(httpError(400))).toBe(true);
    expect(isUnrecoverableCompleteFailure(httpError(410))).toBe(true);
    // A missing-chunk answer and a 409 conflict stay resumable.
    expect(isUnrecoverableCompleteFailure(httpError(400, [1]))).toBe(false);
    expect(isUnrecoverableCompleteFailure(httpError(409))).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// uploadFileChunked against a fake server that implements the NEW protocol
// ---------------------------------------------------------------------------

type ScriptedFailure = { status: number; detail: string; missing?: number[]; markCompleted?: boolean };

type FakeSession = {
  id: string;
  environmentId: string;
  requestId: string;
  filename: string;
  fileSize: number;
  chunkSize: number;
  totalChunks: number;
  status: "active" | "completed";
  received: Set<number>;
  bytes: Uint8Array;
  clipId: string;
};

class FakeServer {
  readonly chunkSize: number;
  readonly sessions = new Map<string, FakeSession>();
  readonly puts: Array<{ index: number; bytes: number }> = [];
  readonly initStatuses: number[] = [];
  chunkFailures = new Map<number, ScriptedFailure[]>();
  completeFailures: ScriptedFailure[] = [];

  constructor(chunkSize = 8) {
    this.chunkSize = chunkSize;
  }

  initCount = 0;
  completeCount = 0;
  deleteCount = 0;

  private clipOf(session: FakeSession): Record<string, unknown> {
    return {
      id: session.clipId,
      type: "file",
      createdAt: 1,
      expiresAt: 2,
      maxDownloads: 10,
      downloadCount: 0,
      accessCode: null,
      accessToken: null,
      payload: {
        text: null,
        file: {
          name: session.filename,
          size: session.fileSize,
          type: "application/octet-stream",
          downloadUrl: `http://localhost/api/clips/${session.clipId}/file`
        }
      },
      directUrl: null
    };
  }

  private infoOf(session: FakeSession): UploadInfoResponse {
    const received = [...session.received].sort((a, b) => a - b);
    const missing = getMissingIndices(session.totalChunks, received);
    return {
      uploadId: session.id,
      filename: session.filename,
      fileSize: session.fileSize,
      mimeType: "application/octet-stream",
      chunkSize: session.chunkSize,
      totalChunks: session.totalChunks,
      receivedChunks: received,
      receivedCount: received.length,
      missingChunks: missing,
      status: session.status,
      createdAt: 1,
      updatedAt: 2,
      expiresAt: Date.now() + 60_000
    };
  }

  private json(payload: unknown, status: number): Response {
    return new Response(JSON.stringify(payload), {
      status,
      headers: { "Content-Type": "application/json" }
    });
  }

  handle = async (input: string, init?: RequestInit): Promise<Response> => {
    const url = new URL(input, "http://localhost");
    const method = (init?.method ?? "GET").toUpperCase();
    const path = url.pathname;

    if (path === "/api/uploads/init" && method === "POST") {
      this.initCount += 1;
      const body = JSON.parse(String(init?.body)) as {
        filename: string;
        fileSize: number;
        environmentId: string;
        requestId: string;
      };
      const scripted = this.initStatuses.shift();
      if (scripted) {
        return this.json({ detail: "上一个上传会话仍在处理中，请稍后重试" }, scripted);
      }
      const id = `upload-${this.initCount}`;
      const session: FakeSession = {
        id,
        environmentId: body.environmentId,
        requestId: body.requestId,
        filename: body.filename,
        fileSize: body.fileSize,
        chunkSize: this.chunkSize,
        totalChunks: body.fileSize === 0 ? 0 : Math.ceil(body.fileSize / this.chunkSize),
        status: "active",
        received: new Set(),
        bytes: new Uint8Array(body.fileSize),
        clipId: `clip-${this.initCount}`
      };
      this.sessions.set(id, session);
      return this.json(
        {
          uploadId: id,
          chunkSize: session.chunkSize,
          totalChunks: session.totalChunks,
          fileSize: session.fileSize,
          filename: session.filename,
          mimeType: "application/octet-stream",
          expiresAt: Date.now() + 60_000,
          status: "active",
          receivedChunks: []
        },
        201
      );
    }

    const chunkMatch = /^\/api\/uploads\/([^/]+)\/chunks\/(\d+)$/.exec(path);
    if (chunkMatch && method === "PUT") {
      const session = this.sessions.get(chunkMatch[1]);
      if (!session) return this.json({ detail: "上传会话不存在" }, 404);
      // A completed/completing session refuses further chunk writes.
      if (session.status === "completed") {
        return this.json({ detail: "上传已完成" }, 409);
      }
      const index = Number.parseInt(chunkMatch[2], 10);
      const expected = Math.min(
        session.chunkSize,
        session.fileSize - index * session.chunkSize
      );
      const payload = new Uint8Array(await (init?.body as Blob).arrayBuffer());
      this.puts.push({ index, bytes: payload.length });

      const scripted = this.chunkFailures.get(index)?.shift();
      if (scripted) {
        if (scripted.markCompleted) session.status = "completed";
        return this.json(
          { detail: scripted.detail, ...(scripted.missing ? { missing: scripted.missing } : {}) },
          scripted.status
        );
      }

      // The server's size contract: every chunk but the last is exactly
      // chunkSize bytes; anything else is a parameter error.
      if (payload.length !== expected) {
        return this.json(
          {
            detail: `分片大小不匹配：第 ${index} 块应为 ${expected} 字节，实际声明 ${payload.length} 字节`
          },
          400
        );
      }

      session.bytes.set(payload, index * session.chunkSize);
      session.received.add(index);
      const received = [...session.received].sort((a, b) => a - b);
      return this.json(
        {
          uploadId: session.id,
          index,
          receivedCount: received.length,
          totalChunks: session.totalChunks,
          receivedChunks: received,
          complete: received.length >= session.totalChunks
        },
        200
      );
    }

    if (/^\/api\/uploads\/[^/]+$/.test(path) && method === "GET") {
      const session = this.sessions.get(path.split("/").pop() as string);
      if (!session) return this.json({ detail: "上传会话不存在" }, 404);
      return this.json(this.infoOf(session), 200);
    }

    if (/^\/api\/uploads\/[^/]+$/.test(path) && method === "DELETE") {
      this.deleteCount += 1;
      this.sessions.delete(path.split("/").pop() as string);
      return this.json({ ok: true }, 200);
    }

    if (/^\/api\/uploads\/[^/]+\/complete$/.test(path) && method === "POST") {
      const id = path.split("/")[3];
      const session = this.sessions.get(id);
      if (!session) return this.json({ detail: "上传会话不存在" }, 404);
      this.completeCount += 1;

      if (session.status === "completed") {
        // Idempotent replay of the clip created by the first complete.
        return this.json(this.clipOf(session), 200);
      }
      const scripted = this.completeFailures.shift();
      if (scripted) {
        return this.json(
          { detail: scripted.detail, ...(scripted.missing ? { missing: scripted.missing } : {}) },
          scripted.status
        );
      }
      const missing = getMissingIndices(session.totalChunks, [...session.received]);
      if (missing.length > 0) {
        return this.json({ detail: "分片缺失，请续传后重试", missing }, 400);
      }
      session.status = "completed";
      return this.json(this.clipOf(session), 201);
    }

    return this.json({ detail: `unexpected ${method} ${path}` }, 500);
  };
}

const makeBytes = (size: number): Uint8Array<ArrayBuffer> => {
  const bytes = new Uint8Array(new ArrayBuffer(size));
  for (let i = 0; i < size; i += 1) bytes[i] = i % 251;
  return bytes;
};

const makeFile = (size: number, name = "demo.bin"): File =>
  new File([makeBytes(size)], name, { type: "application/octet-stream" });

const runUpload = async (
  server: FakeServer,
  file: File,
  options: { progress?: UploadProgress[]; onProgress?: (p: UploadProgress) => void } = {}
) => {
  vi.stubGlobal("fetch", (input: string, init?: RequestInit) => server.handle(input, init));
  return uploadFileChunked({
    file,
    filename: file.name,
    clip: { environmentId: "env-1", expiresAt: Date.now() + 3600_000 },
    onProgress: (progress) => {
      options.progress?.push({ ...progress });
      options.onProgress?.(progress);
    }
  });
};

describe("uploadFileChunked (pre-allocated single file, DB-owned progress)", () => {
  it("writes every range in place with exact sizes and completes without any merge", async () => {
    const server = new FakeServer(8);
    const file = makeFile(20);
    const progress: UploadProgress[] = [];

    const clip = await runUpload(server, file, { progress });

    expect(server.initCount).toBe(1);
    expect(server.completeCount).toBe(1);
    // 20 bytes / 8 = chunks 0,1 (8 bytes) + chunk 2 (4 bytes, the remainder).
    expect(server.puts.map((put) => put.index).sort((a, b) => a - b)).toEqual([0, 1, 2]);
    expect(server.puts.map((put) => put.bytes).sort((a, b) => a - b)).toEqual([4, 8, 8]);

    const session = [...server.sessions.values()][0];
    expect(Buffer.from(session.bytes).equals(Buffer.from(makeBytes(20)))).toBe(true);
    expect(clip.id).toBe("clip-1");
    expect(clip.type).toBe("file");

    // The client never announces a merge/assembly phase any more.
    const statuses = progress.map((entry) => entry.status);
    expect(statuses).toContain("finalizing");
    expect(statuses).not.toContain("assembling");
    expect(statuses.at(-1)).toBe("completed");
  });

  it("fails fast (no retry storm) when the server refuses a chunk size", async () => {
    const server = new FakeServer(8);
    server.chunkFailures.set(1, [
      { status: 400, detail: "分片大小不匹配：第 1 块应为 8 字节，实际声明 7 字节" }
    ]);
    const progress: UploadProgress[] = [];

    await expect(runUpload(server, makeFile(20), { progress })).rejects.toMatchObject({
      status: 400
    });
    expect(progress.at(-1)?.status).toBe("failed");
    // A parameter error is not retried: exactly one PUT for that index.
    expect(server.puts.filter((put) => put.index === 1)).toHaveLength(1);
    expect(server.completeCount).toBe(0);
  });

  it("re-sends exactly the ranges named by a 409 reset and finishes in the same session", async () => {
    const server = new FakeServer(8);
    server.chunkFailures.set(0, [
      { status: 409, detail: "上传文件已丢失，请重新上传全部分片", missing: [0, 1, 2] }
    ]);
    const progress: UploadProgress[] = [];

    const clip = await runUpload(server, makeFile(20), { progress });

    // Same session, no re-init.
    expect(server.initCount).toBe(1);
    expect(server.completeCount).toBe(1);
    expect(clip.id).toBe("clip-1");
    // Every range was sent twice: once before the reset, once after.
    for (const index of [0, 1, 2]) {
      expect(server.puts.filter((put) => put.index === index)).toHaveLength(2);
    }
    expect(progress.map((entry) => entry.status)).toContain("restarting");
  });

  it("jumps straight to complete when a PUT reports the session already completed", async () => {
    const server = new FakeServer(8);
    server.chunkFailures.set(1, [
      { status: 409, detail: "上传已完成", markCompleted: true }
    ]);
    const progress: UploadProgress[] = [];

    const clip = await runUpload(server, makeFile(20), { progress });

    // Progress was proven finished by the probe; complete replays the clip.
    expect(server.completeCount).toBe(1);
    expect(clip.id).toBe("clip-1");
    expect(progress.at(-1)?.status).toBe("completed");
    // No re-init and no reset round: the probe proved the DB says "done".
    expect(server.initCount).toBe(1);
    expect(server.deleteCount).toBe(0);
  });

  it("re-uploads only the ranges COMPLETE reports missing, then completes", async () => {
    const server = new FakeServer(8);
    server.completeFailures.push({
      status: 400,
      detail: "分片缺失，请续传后重试",
      missing: [2]
    });

    const clip = await runUpload(server, makeFile(20));

    expect(server.completeCount).toBe(2);
    expect(server.puts.filter((put) => put.index === 2)).toHaveLength(2);
    expect(clip.id).toBe("clip-1");
  });

  it("rethinks a 409 from COMPLETE as a retry (no assembly, session stays resumable)", async () => {
    const server = new FakeServer(8);
    server.completeFailures.push({ status: 409, detail: "上传正在完成，请稍后查询" });

    const clip = await runUpload(server, makeFile(20));

    expect(server.completeCount).toBe(2);
    expect(server.deleteCount).toBe(0);
    expect(clip.id).toBe("clip-1");
  });

  it("drops the session (and its pre-allocated file) when COMPLETE answers 410", async () => {
    const server = new FakeServer(8);
    server.completeFailures.push({ status: 410, detail: "上传会话已过期，请重新上传" });

    await expect(runUpload(server, makeFile(20))).rejects.toMatchObject({ status: 410 });
    expect(server.deleteCount).toBe(1);
    expect([...server.sessions.keys()]).toHaveLength(0);
  });

  it("retries INIT when the idempotency key is still held (409)", async () => {
    const server = new FakeServer(8);
    server.initStatuses.push(409);

    const clip = await runUpload(server, makeFile(12));

    expect(server.initCount).toBe(2);
    expect(clip.id).toBe("clip-2");
  });

  it("sends an empty file straight to complete (nothing to upload)", async () => {
    const server = new FakeServer(8);

    const clip = await runUpload(server, makeFile(0, "empty.bin"));

    expect(server.puts).toHaveLength(0);
    expect(server.completeCount).toBe(1);
    expect(clip.id).toBe("clip-1");
  });

  it("keeps the resume slot after a parameter error so a later retry can continue", async () => {
    const server = new FakeServer(8);
    const file = makeFile(20);
    const resumeKey = buildResumeKey({
      environmentId: "env-1",
      filename: file.name,
      fileSize: file.size,
      lastModified: file.lastModified
    });
    server.chunkFailures.set(1, [{ status: 400, detail: "分片大小不匹配" }]);

    await expect(runUpload(server, file)).rejects.toMatchObject({ status: 400 });
    // Still resumable: only a 400/410 from COMPLETE drops the slot.
    expect(storage.getItem(resumeKey)).not.toBeNull();

    clearStoredUploadId(resumeKey);
    expect(storage.getItem(resumeKey)).toBeNull();
  });
});

describe("upload progress model", () => {
  it("never exposes an assembling/merging state", async () => {
    const server = new FakeServer(4);
    const progress: UploadProgress[] = [];
    await runUpload(server, makeFile(10), { progress });
    const serialised = JSON.stringify(progress);
    expect(serialised).not.toContain("assembling");
    expect(serialised).toContain("finalizing");
  });

  it("reports server-owned progress (chunks + bytes) while streaming", async () => {
    const server = new FakeServer(8);
    const progress: UploadProgress[] = [];
    await runUpload(server, makeFile(20), { progress });
    const uploading = progress.filter((entry) => entry.status === "uploading");
    expect(uploading.length).toBeGreaterThan(0);
    expect(uploading.at(-1)?.uploadedChunks).toBe(3);
    expect(uploading.at(-1)?.uploadedBytes).toBe(20);
    expect(uploading.at(-1)?.percent).toBe(100);
  });
});
