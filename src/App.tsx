import { ChangeEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  useClipboardStore,
  ClipType,
  RemoteClip
} from "./store/useClipboardStore";
import { readFromClipboard, writeToClipboard } from "./utils/clipboard";
import {
  generateAccessCode,
  generateToken,
  hoursToMilliseconds,
  formatBytes
} from "./utils/generators";
import {
  listRemoteClips,
  createRemoteClip,
  deleteRemoteClip,
  fetchRemoteClip,
  registerPersistentToken,
  fetchAppConfig,
  type AppConfig
} from "./utils/api";
import { uploadFileChunked, type UploadProgress } from "./utils/uploads";
import "./App.css";
import { useI18n } from "./i18n/I18nProvider";
import type { Locale } from "./i18n/locales";
import Captcha, { CaptchaProvider as CaptchaProviderType } from "./components/Captcha";

const buildRelativeAccessPath = (value: string): string => {
  const trimmed = value.trim();
  if (!trimmed) {
    return "";
  }
  if (typeof window === "undefined") {
    return `/${trimmed}`;
  }
  const target = new URL(`./${trimmed}`, window.location.href);
  return target.toString();
};

type ToastState = {
  kind: "success" | "error" | "info";
  message: string;
};

const SETTINGS_STORAGE_KEY = "super-clipboard::settings";
const THEME_STORAGE_KEY = "super-clipboard::theme";
const MIN_EXPIRY_HOURS = 1;
const MAX_EXPIRY_HOURS = 120;
const DEFAULT_EXPIRY_HOURS = 24;
const MAX_FILE_SIZE_BYTES = 50 * 1024 * 1024; // 50MB
const TOKEN_EXPIRY_MS = 720 * 60 * 60 * 1000; // 720 hours
const DEFAULT_MAX_DOWNLOADS = 10;
const MAX_DOWNLOADS_OPTIONS = [3, 5, 10, 20, 50, 100];
const SUPPORTED_CAPTCHA_PROVIDERS: readonly CaptchaProviderType[] = ["turnstile", "recaptcha"] as const;

type DraftFile = {
  file: File;
  name: string;
  size: number;
  type: string;
};

type ThemeMode = "light" | "dark";

const getInitialThemeMode = (): ThemeMode => {
  if (typeof window === "undefined") {
    return "light";
  }

  try {
    const stored = window.localStorage.getItem(THEME_STORAGE_KEY);
    if (stored === "light" || stored === "dark") {
      return stored;
    }

    return window.matchMedia("(prefers-color-scheme: dark)").matches
      ? "dark"
      : "light";
  } catch {
    return "light";
  }
};

const emptyToast: ToastState | null = null;

const App = () => {
  const {
    t,
    formatDateTime,
    formatDuration,
    formatRemaining,
    locale,
    setLocale,
    options
  } = useI18n();
  const {
    remoteClips,
    setRemoteClips,
    upsertRemoteClip,
    updateRemoteClip,
    removeRemoteClip: removeClipFromStore,
    settings,
    updateSettings
  } = useClipboardStore();

  const [type, setType] = useState<ClipType>("text");
  const [textContent, setTextContent] = useState("");
  const [selectedFile, setSelectedFile] = useState<DraftFile | null>(null);
  const [expiresInHours, setExpiresInHours] = useState(DEFAULT_EXPIRY_HOURS);
  const [maxDownloads, setMaxDownloads] = useState(DEFAULT_MAX_DOWNLOADS);
  const [shortCode, setShortCode] = useState(() => generateAccessCode());
  const [accessMode, setAccessMode] = useState<"code" | "token">(
    () => (settings.persistentToken ? "token" : "code")
  );
  const [isSettingsOpen, setIsSettingsOpen] = useState(false);
  const [isCreatingClip, setIsCreatingClip] = useState(false);
  const [uploadProgress, setUploadProgress] = useState<UploadProgress | null>(null);
  const [isCancellingUpload, setIsCancellingUpload] = useState(false);
  const uploadControllerRef = useRef<AbortController | null>(null);

  const [toast, setToast] = useState<ToastState | null>(emptyToast);
  const [isImportingClipboard, setIsImportingClipboard] = useState(false);
  const [now, setNow] = useState(Date.now());
  const [settingsTokenDraft, setSettingsTokenDraft] = useState(
    settings.persistentToken
  );
  const [captchaToken, setCaptchaToken] = useState("");
  const [captchaError, setCaptchaError] = useState<string | null>(null);
  const [captchaResetKey, setCaptchaResetKey] = useState(0);
  const [captchaConfig, setCaptchaConfig] = useState<AppConfig | null>(null);
  const [themeMode, setThemeMode] = useState<ThemeMode>(getInitialThemeMode);

  const isDarkTheme = themeMode === "dark";

  const captchaProvider = useMemo<CaptchaProviderType | null>(() => {
    if (!captchaConfig?.captchaProvider) {
      return null;
    }
    const raw = captchaConfig.captchaProvider.trim().toLowerCase();
    return SUPPORTED_CAPTCHA_PROVIDERS.find((item) => item === raw) ?? null;
  }, [captchaConfig?.captchaProvider]);

  const captchaSiteKey = useMemo(
    () => (captchaConfig?.captchaSiteKey ?? "").trim(),
    [captchaConfig?.captchaSiteKey]
  );
  const isCaptchaEnabled = Boolean(captchaProvider && captchaSiteKey);

  useEffect(() => {
    document.documentElement.dataset.theme = themeMode;
    try {
      window.localStorage.setItem(THEME_STORAGE_KEY, themeMode);
    } catch (error) {
      console.warn("Failed to persist theme preference:", error);
    }
  }, [themeMode]);

  const handleToggleTheme = () => {
    setThemeMode((current) => (current === "dark" ? "light" : "dark"));
  };

  useEffect(() => {
    try {
      const rawSettings = window.localStorage.getItem(SETTINGS_STORAGE_KEY);
      if (rawSettings) {
        const parsed = JSON.parse(rawSettings) as Partial<{
          persistentToken: unknown;
          tokenUpdatedAt: unknown;
          tokenLastUsedAt: unknown;
          environmentId: unknown;
        }>;
        const sanitized = {
          persistentToken:
            typeof parsed.persistentToken === "string" ? parsed.persistentToken : "",
          tokenUpdatedAt:
            typeof parsed.tokenUpdatedAt === "number" ? parsed.tokenUpdatedAt : null,
          tokenLastUsedAt:
            typeof parsed.tokenLastUsedAt === "number" ? parsed.tokenLastUsedAt : null,
          environmentId:
            typeof parsed.environmentId === "string" && parsed.environmentId
              ? parsed.environmentId
              : crypto.randomUUID()
        };
        updateSettings(sanitized);
      }
    } catch (error) {
      console.warn("Failed to load environment settings:", error);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    let cancelled = false;

    const loadRemoteClips = async () => {
      if (!settings.environmentId) {
        return;
      }
      try {
        const clips = await listRemoteClips(settings.environmentId);
        if (!cancelled) {
          setRemoteClips(clips);
        }
      } catch (error) {
        console.warn(t("toast.loadFailed"), error);
        if (!cancelled) {
          setToast({ kind: "error", message: t("toast.loadFailed") });
        }
      }
    };

    loadRemoteClips();
    const timer = window.setInterval(loadRemoteClips, 60_000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [setRemoteClips, settings.environmentId, t]);

  useEffect(() => {
    window.localStorage.setItem(SETTINGS_STORAGE_KEY, JSON.stringify(settings));
  }, [settings]);

  useEffect(() => {
    setSettingsTokenDraft(settings.persistentToken);
  }, [settings.persistentToken]);

  useEffect(() => {
    if (!toast) {
      return;
    }
    const timer = window.setTimeout(() => setToast(emptyToast), 2600);
    return () => window.clearTimeout(timer);
  }, [toast]);

  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 30000);
    return () => window.clearInterval(timer);
  }, []);

  useEffect(() => {
    const loadConfig = async () => {
      try {
        const config = await fetchAppConfig();
        setCaptchaConfig(config);
      } catch (error) {
        console.warn("Failed to load app config", error);
      }
    };
    void loadConfig();
  }, []);

  useEffect(() => {
    if (!settings.persistentToken) {
      return;
    }
    const reference = settings.tokenLastUsedAt ?? settings.tokenUpdatedAt;
    if (!reference) {
      return;
    }
    if (now - reference >= TOKEN_EXPIRY_MS) {
      updateSettings({
        persistentToken: "",
        tokenUpdatedAt: null,
        tokenLastUsedAt: null,
      });
      setAccessMode("code");
      setSettingsTokenDraft("");
      setToast({
        kind: "info",
        message: t("toast.tokenExpired")
      });
    }
  }, [
    now,
    settings.persistentToken,
    settings.tokenLastUsedAt,
    settings.tokenUpdatedAt,
    t,
    updateSettings
  ]);

  useEffect(() => {
    if (!settings.persistentToken && accessMode === "token") {
      setAccessMode("code");
    }
  }, [settings.persistentToken, accessMode]);

  const hasActiveClips = useMemo(
    () =>
      remoteClips.some(
        (clip) => clip.expiresAt > now && clip.downloadCount < clip.maxDownloads
      ),
    [remoteClips, now]
  );

  const resetForm = () => {
    setTextContent("");
    setSelectedFile(null);
    setExpiresInHours(DEFAULT_EXPIRY_HOURS);
    setMaxDownloads(DEFAULT_MAX_DOWNLOADS);
    setShortCode(generateAccessCode());
    setType("text");
    if (!settings.persistentToken) {
      setAccessMode("code");
    }
  };

  const resetCaptcha = () => {
    setCaptchaToken("");
    setCaptchaError(null);
    setCaptchaResetKey((value) => value + 1);
  };

  const handleImportClipboard = async () => {
    setIsImportingClipboard(true);
    const fromSystem = await readFromClipboard();
    setIsImportingClipboard(false);
    if (!fromSystem) {
      setToast({
        kind: "error",
        message: t("toast.clipboardReadFailed")
      });
      return;
    }
    setType("text");
    setTextContent(fromSystem);
    setToast({
      kind: "success",
      message: t("toast.clipboardImported")
    });
  };

  const effectiveMaxFileSize = useMemo(() => {
    const serverMax = captchaConfig?.maxFileSizeBytes;
    if (typeof serverMax === "number" && Number.isFinite(serverMax) && serverMax > 0) {
      return serverMax;
    }
    return MAX_FILE_SIZE_BYTES;
  }, [captchaConfig?.maxFileSizeBytes]);

  const processFile = useCallback(
    async (
      file: File,
      options?: {
        fallbackName?: string;
      }
    ): Promise<boolean> => {
      // Chunked upload: keep the File handle (sliced per chunk on demand)
      // instead of base64-ing the whole file into memory (old dataUrl path).
      if (file.size > effectiveMaxFileSize) {
        setToast({
          kind: "error",
          message: t("toast.fileTooLarge")
        });
        setSelectedFile(null);
        return false;
      }

      const resolvedName =
        (file.name && file.name.trim()) || options?.fallbackName || "clipboard-upload";

      try {
        setSelectedFile({
          file,
          name: resolvedName,
          size: file.size,
          type: file.type
        });
        return true;
      } catch (error) {
        console.warn(t("toast.fileReadFailed"), error);
        setToast({
          kind: "error",
          message: t("toast.fileReadFailed")
        });
        setSelectedFile(null);
        return false;
      }
    },
    [effectiveMaxFileSize, t]
  );

  useEffect(() => {
    if (type !== "file") {
      return;
    }

    const handlePaste = async (event: ClipboardEvent) => {
      const clipboardData = event.clipboardData;
      if (!clipboardData) {
        return;
      }

      const items = clipboardData.items;
      if (!items || items.length === 0) {
        return;
      }

      const item = Array.from(items).find(
        (candidate) =>
          candidate.kind === "file" && candidate.type.startsWith("image/")
      );

      if (!item) {
        return;
      }

      const blob = item.getAsFile();
      if (!blob) {
        return;
      }

      event.preventDefault();

      const mimeType = blob.type || item.type || "image/png";
      const extensionCandidate = mimeType.split("/")[1]?.split(";")[0] ?? "";
      const safeExtension = extensionCandidate ? extensionCandidate : "png";
      const fallbackName = `pasted-image-${new Date()
        .toISOString()
        .replace(/[:.]/g, "-")}.${safeExtension}`;

      const file =
        blob instanceof File && blob.name
          ? blob
          : new File([blob], fallbackName, {
              type: mimeType,
              lastModified: Date.now()
            });

      const success = await processFile(file, { fallbackName });
      if (success) {
        setToast({
          kind: "success",
          message: t("toast.clipboardImageImported")
        });
      }
    };

    window.addEventListener("paste", handlePaste);
    return () => {
      window.removeEventListener("paste", handlePaste);
    };
  }, [processFile, t, type]);

  const handleFileSelect = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    if (!file) {
      setSelectedFile(null);
      return;
    }

    const success = await processFile(file);
    if (!success) {
      event.target.value = "";
    }
  };

  const handleCancelUpload = () => {
    const ctrl = uploadControllerRef.current;
    if (!ctrl || isCancellingUpload) return;
    setIsCancellingUpload(true);
    ctrl.abort();
  };

  const describeUploadStatus = (progress: UploadProgress): string => {
    switch (progress.status) {
      case "initializing":
        return t("upload.initializing");
      case "resuming":
        return t("upload.resuming");
      case "retrying":
        return t("upload.retrying", {
          index: progress.failedIndex ?? 0,
          attempt: progress.attempt ?? 1,
          max: progress.maxAttempts ?? 1
        });
      case "assembling":
        return t("upload.assembling");
      case "cancelled":
        return t("upload.cancelled");
      case "failed":
        return t("upload.failed");
      case "completed":
        return t("toast.createSuccess.file");
      case "uploading":
      default:
        return t("upload.uploading");
    }
  };

  const handleCreateRemoteClip = async () => {
    if (isCreatingClip) {
      return;
    }
    const trimmedText = textContent.trim();
    if (type === "text" && !trimmedText) {
      setToast({
        kind: "info",
        message: t("toast.textRequired")
      });
      return;
    }

    if (type === "file" && !selectedFile) {
      setToast({
        kind: "info",
        message: t("toast.fileRequired")
      });
      return;
    }

    if (
      Number.isNaN(expiresInHours) ||
      expiresInHours < MIN_EXPIRY_HOURS ||
      expiresInHours > MAX_EXPIRY_HOURS
    ) {
      setToast({
        kind: "info",
        message: t("toast.expiryInvalid")
      });
      return;
    }

    if (Number.isNaN(maxDownloads) || maxDownloads < 1 || maxDownloads > 500) {
      setToast({
        kind: "info",
        message: t("toast.downloadLimitInvalid")
      });
      return;
    }

    const nowTs = Date.now();
    const tokenValue = settings.persistentToken.trim();
    const usingToken = accessMode === "token";
    const activeShortCode = accessMode === "code" ? shortCode.trim() : "";

    if (usingToken) {
      if (!tokenValue) {
        setToast({
          kind: "info",
          message: t("toast.tokenRequired")
        });
        setAccessMode("code");
        return;
      }

      if (!settings.environmentId) {
        setToast({
          kind: "info",
          message: t("toast.tokenRequired")
        });
        setAccessMode("code");
        return;
      }

      const reference = settings.tokenLastUsedAt ?? settings.tokenUpdatedAt;
      if (reference && nowTs - reference >= TOKEN_EXPIRY_MS) {
        updateSettings({
          persistentToken: "",
          tokenUpdatedAt: null,
          tokenLastUsedAt: null,
        });
        setAccessMode("code");
        setSettingsTokenDraft("");
        setToast({
          kind: "info",
          message: t("toast.tokenExpired")
        });
        return;
      }

      if (tokenValue.length < 7) {
        setToast({
          kind: "info",
          message: t("toast.tokenTooShort")
        });
        return;
      }
    } else {
      if (!/^\d{5}$/.test(activeShortCode)) {
        setToast({
          kind: "info",
          message: t("toast.shortCodeInvalid")
        });
        return;
      }
    }

    if (isCaptchaEnabled && !captchaToken) {
      setToast({
        kind: "info",
        message: t("toast.captchaRequired")
      });
      return;
    }

    // File path: chunked multi-POST upload (no giant base64 body).
    if (type === "file" && selectedFile) {
      const controller = new AbortController();
      uploadControllerRef.current = controller;
      setIsCreatingClip(true);
      setIsCancellingUpload(false);
      setUploadProgress({
        uploadId: null,
        uploadedBytes: 0,
        totalBytes: selectedFile.size,
        uploadedChunks: 0,
        totalChunks: 0,
        percent: 0,
        status: "initializing"
      });
      try {
        const created = await uploadFileChunked({
          file: selectedFile.file,
          filename: selectedFile.name,
          // environmentId + expiresAt are required: every upload becomes a file
          // clip, owned by this environment (that is how a nameless clip
          // without access code/token is listed).
          clip: {
            environmentId: settings.environmentId,
            expiresAt: Date.now() + hoursToMilliseconds(expiresInHours),
            maxDownloads,
            accessCode: accessMode === "code" ? activeShortCode : undefined,
            accessToken: usingToken ? tokenValue : undefined
          },
          // Captcha and the clip params are sent at init so they are validated
          // before any chunk consumes storage; complete only assembles the
          // already-validated session.
          captchaToken: isCaptchaEnabled ? captchaToken : undefined,
          captchaProvider: captchaProvider ?? undefined,
          concurrency: 3,
          signal: controller.signal,
          onProgress: (p) => setUploadProgress({ ...p })
        });

        upsertRemoteClip(created);
        setToast({
          kind: "success",
          message: t("toast.createSuccess.file")
        });

        if (usingToken) {
          updateSettings({
            tokenLastUsedAt: nowTs
          });
        }

        if (accessMode === "code") {
          setShortCode(generateAccessCode());
        }
        resetForm();
        setUploadProgress(null);
      } catch (error) {
        if (
          (error instanceof DOMException && error.name === "AbortError") ||
          (error instanceof Error && error.message.toLowerCase().includes("cancel"))
        ) {
          setToast({ kind: "info", message: t("toast.uploadCancelled") });
          setUploadProgress((prev) =>
            prev ? { ...prev, status: "cancelled" } : prev
          );
        } else {
          const status = (error as Error & { status?: number }).status;
          const isTimeout =
            (error as Error & { timeout?: boolean }).timeout === true ||
            (error instanceof Error && error.message.toLowerCase().includes("timeout"));
          if (status === 410) {
            setToast({ kind: "error", message: t("toast.uploadSessionExpired") });
          } else if (isTimeout) {
            setToast({ kind: "error", message: t("toast.uploadTimeout") });
          } else {
            const reason =
              error instanceof Error ? error.message : t("toast.createFailed");
            // Network drop without status: hint that retry resumes.
            if (status === undefined && reason && /failed to fetch|network|load failed/i.test(reason)) {
              setToast({ kind: "error", message: t("upload.networkError") });
            } else {
              setToast({
                kind: "error",
                message: t("toast.uploadFailed", { reason })
              });
            }
          }
          setUploadProgress((prev) =>
            prev ? { ...prev, status: "failed" } : prev
          );
        }
      } finally {
        uploadControllerRef.current = null;
        setIsCreatingClip(false);
        setIsCancellingUpload(false);
        if (isCaptchaEnabled) {
          resetCaptcha();
        }
      }
      return;
    }

    setIsCreatingClip(true);
    try {
      const created = await createRemoteClip({
        type,
        expiresAt: Date.now() + hoursToMilliseconds(expiresInHours),
        maxDownloads,
        environmentId: settings.environmentId,
        accessCode: accessMode === "code" ? activeShortCode : undefined,
        accessToken: usingToken ? tokenValue : undefined,
        captchaToken: isCaptchaEnabled ? captchaToken : undefined,
        captchaProvider: captchaProvider ?? undefined,
        payload: { text: trimmedText }
      });

      upsertRemoteClip(created);
      setToast({
        kind: "success",
        message: t("toast.createSuccess.text")
      });

      if (usingToken) {
        updateSettings({
          tokenLastUsedAt: nowTs
        });
      }

      if (accessMode === "code") {
        setShortCode(generateAccessCode());
      }
      resetForm();
    } catch (error) {
      const message =
        error instanceof Error ? error.message : t("toast.createFailed");
      setToast({ kind: "error", message });
    } finally {
      setIsCreatingClip(false);
      if (isCaptchaEnabled) {
        resetCaptcha();
      }
    }
  };

  const handleCopyAccess = async (
    value: string,
    target: "direct-link" | "token"
  ) => {
    const label =
      target === "direct-link"
        ? t("copy.target.directLink")
        : t("copy.target.token");
    if (!value) {
      setToast({
        kind: "error",
        message: t("toast.noAccessValue", { target: label })
      });
      return;
    }
    const ok = await writeToClipboard(value);
    setToast(
      ok
        ? { kind: "success", message: t("toast.copySuccess", { target: label }) }
        : { kind: "error", message: t("toast.copyFailed", { target: label }) }
    );
  };

  const refreshRemoteClip = async (clipId: string) => {
    try {
      const fresh = await fetchRemoteClip(clipId, settings.environmentId);
      updateRemoteClip(clipId, fresh);
    } catch (error) {
      const message =
        error instanceof Error ? error.message : t("toast.clipAutoDeleted");
      if (/未找到|不存在|过期|销毁/.test(message)) {
        removeClipFromStore(clipId);
        setToast({ kind: "info", message: t("toast.clipAutoDeleted") });
      } else {
        console.warn(t("toast.loadFailed"), error);
      }
    }
  };

  const handleDownloadFile = (clip: RemoteClip) => {
    const file = clip.payload.file;
    if (!file) {
      setToast({
        kind: "error",
        message: t("toast.fileMissing")
      });
      return;
    }

    const link = document.createElement("a");
    link.href = file.downloadUrl;
    link.rel = "noopener";
    link.target = "_blank";
    link.click();

    setToast({
      kind: "success",
      message: t("toast.fileDownloadStarted")
    });

    window.setTimeout(() => {
      void refreshRemoteClip(clip.id);
    }, 800);
  };

  const handleCopyRemoteText = async (clip: RemoteClip) => {
    const text = clip.payload.text ?? "";
    if (!text) {
      setToast({
        kind: "error",
        message: t("toast.clipEmpty")
      });
      return;
    }

    const ok = await writeToClipboard(text);
    setToast(
      ok
        ? {
            kind: "success",
            message: t("toast.copySuccess", { target: t("copy.target.text") })
          }
        : {
            kind: "error",
            message: t("toast.copyFailed", { target: t("copy.target.text") })
          }
    );
  };

  const handleRemoveRemoteClip = async (clipId: string) => {
    try {
      await deleteRemoteClip(clipId, settings.environmentId);
      removeClipFromStore(clipId);
      setToast({
        kind: "info",
        message: t("toast.clipDeleted")
      });
    } catch (error) {
      const message =
        error instanceof Error ? error.message : t("toast.removeFailed");
      setToast({ kind: "error", message });
    }
  };

  const handleOpenSettings = () => {
    setSettingsTokenDraft(settings.persistentToken);
    setIsSettingsOpen(true);
  };

  const handleCloseSettings = () => {
    setIsSettingsOpen(false);
    setSettingsTokenDraft(settings.persistentToken);
  };

  const handleGeneratePersistentToken = () => {
    setSettingsTokenDraft(generateToken(20));
  };

  const handleSaveSettings = async () => {
    const trimmedToken = settingsTokenDraft.trim();
    if (trimmedToken && trimmedToken.length < 7) {
      setToast({
        kind: "info",
        message: t("toast.tokenTooShort")
      });
      return;
    }

    if (!trimmedToken) {
      updateSettings({
        persistentToken: "",
        tokenUpdatedAt: null,
        tokenLastUsedAt: null,
      });
      setAccessMode("code");
      setSettingsTokenDraft("");
      setToast({
        kind: "success",
        message: t("toast.settingsSaved")
      });
      setIsSettingsOpen(false);
      return;
    }

    try {
      const registration = await registerPersistentToken(
        trimmedToken,
        settings.environmentId
      );
      updateSettings({
        persistentToken: trimmedToken,
        tokenUpdatedAt: registration.updatedAt,
        tokenLastUsedAt: registration.lastUsedAt ?? null,
        environmentId: registration.environmentId || settings.environmentId
      });
      setSettingsTokenDraft(trimmedToken);
      setToast({
        kind: "success",
        message: t("toast.settingsSaved")
      });
      setIsSettingsOpen(false);
    } catch (error) {
      const reason = error instanceof Error ? error.message : "";
      setToast({
        kind: "error",
        message: reason
          ? t("toast.tokenRegisterFailed", { reason })
          : t("toast.tokenRegisterFailedFallback")
      });
    }
  };

  const tokenReferenceTime = settings.tokenLastUsedAt ?? settings.tokenUpdatedAt;
  const tokenLastActivityLabel = settings.persistentToken
    ? settings.tokenLastUsedAt
      ? t("token.lastUsed", {
          timestamp: formatDateTime(settings.tokenLastUsedAt)
        })
      : settings.tokenUpdatedAt
      ? t("token.updatedAt", {
          timestamp: formatDateTime(settings.tokenUpdatedAt)
        })
      : null
    : null;
  const tokenExpiryNotice =
    settings.persistentToken && tokenReferenceTime
      ? t("token.expiryNotice", {
          duration: formatDuration(
            Math.max(0, TOKEN_EXPIRY_MS - (now - tokenReferenceTime))
          )
        })
      : null;

  const listSummary = remoteClips.length
    ? hasActiveClips
      ? t("list.summaryActive", { count: remoteClips.length })
      : t("list.summaryAllInactive")
    : t("list.summaryEmpty");

  const getClipTitle = (clip: RemoteClip): string => {
    if (clip.type === "text") {
      const text = (clip.payload.text ?? "").replace(/\s+/g, " ").trim();
      if (!text) {
        return t("list.clipType.text");
      }
      return text.length > 32 ? `${text.slice(0, 32)}…` : text;
    }
    return clip.payload.file?.name ?? t("list.clipType.file");
  };

  return (
    <div className="app-shell">
      <header className="site-header">
        <div className="site-header-inner">
          <div className="site-brand" aria-label="Super Clipboard">
            <span className="site-brand-logo">SC</span>
            <span className="site-brand-main">Super Clipboard</span>
            <span className="site-brand-sub">Cloud link workspace</span>
          </div>
          <div className="header-actions">
            <label className="sr-only" htmlFor="locale-select">
              {t("locale.switcherLabel")}
            </label>
            <select
              id="locale-select"
              className="language-switcher"
              value={locale}
              onChange={(event) => setLocale(event.target.value as Locale)}
              aria-label={t("locale.switcherLabel")}
            >
              {options.map((option) => (
                <option key={option.value} value={option.value}>
                  {t(option.labelKey)}
                </option>
              ))}
            </select>
            <button
              type="button"
              className="theme-toggle"
              onClick={handleToggleTheme}
              aria-label={
                isDarkTheme ? "Switch to light theme" : "Switch to dark theme"
              }
              title={
                isDarkTheme ? "Switch to light theme" : "Switch to dark theme"
              }
            >
              <svg className="theme-icon" viewBox="0 0 24 24" aria-hidden="true">
                {isDarkTheme ? (
                  <>
                    <circle cx="12" cy="12" r="4" />
                    <path d="M12 2v2" />
                    <path d="M12 20v2" />
                    <path d="m4.93 4.93 1.41 1.41" />
                    <path d="m17.66 17.66 1.41 1.41" />
                    <path d="M2 12h2" />
                    <path d="M20 12h2" />
                    <path d="m6.34 17.66-1.41 1.41" />
                    <path d="m19.07 4.93-1.41 1.41" />
                  </>
                ) : (
                  <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79Z" />
                )}
              </svg>
            </button>
            <button
              type="button"
              className="icon-button settings-trigger"
              onClick={handleOpenSettings}
              aria-label={t("hero.settings")}
              title={t("hero.settings")}
            >
              <span className="sr-only">{t("hero.settings")}</span>
              <svg
                className="settings-trigger__icon"
                viewBox="0 0 48 48"
                role="img"
                aria-hidden="true"
              >
                <path
                  d="M24 29C26.7614 29 29 26.7614 29 24C29 21.2386 26.7614 19 24 19C21.2386 19 19 21.2386 19 24C19 26.7614 21.2386 29 24 29Z"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="4"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
                <path
                  d="M8.22182 18.2957C8.07786 19.1589 8 20.0678 8 21C8 21.9322 8.07786 22.8411 8.22182 23.7043L5.09131 26.4268C4.79584 26.679 4.73849 27.184 4.9641 27.5236L7.9641 32.0536C8.1897 32.3933 8.65243 32.5116 9.00876 32.3066L12.4196 30.3885C13.6503 31.3354 15.0182 32.084 16.4816 32.5875L17.0206 36.3415C17.0786 36.7426 17.4145 37.0412 17.807 37.0412H30.193C30.5855 37.0412 30.9214 36.7426 30.9794 36.3415L31.5184 32.5875C32.9818 32.084 34.3497 31.3354 35.5804 30.3885L38.9912 32.3066C39.3476 32.5116 39.8103 32.3933 40.0359 32.0536L43.0359 27.5236C43.2615 27.184 43.2042 26.679 42.9087 26.4268L39.7782 23.7043C39.9221 22.8411 40 21.9322 40 21C40 20.0678 39.9221 19.1589 39.7782 18.2957L42.9087 15.5732C43.2042 15.321 43.2615 14.816 43.0359 14.4764L40.0359 9.94643C39.8103 9.60672 39.3476 9.48843 38.9912 9.69343L35.5804 11.6115C34.3497 10.6646 32.9818 9.91602 31.5184 9.41253L30.9794 5.65846C30.9214 5.25735 30.5855 4.95874 30.193 4.95874H17.807C17.4145 4.95874 17.0786 5.25735 17.0206 5.65846L16.4816 9.41253C15.0182 9.91602 13.6503 10.6646 12.4196 11.6115L9.00876 9.69343C8.65243 9.48843 8.1897 9.60672 7.9641 9.94643L4.9641 14.4764C4.73849 14.816 4.79584 15.321 5.09131 15.5732L8.22182 18.2957Z"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="4"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
              </svg>
            </button>
          </div>
        </div>
      </header>

      <main className="page-shell">
        <section className="hero-panel">
          <div className="hero-panel__copy">
            <span className="hero-panel__eyebrow">Super Clipboard</span>
            <h1>{t("hero.title")}</h1>
            <p>{t("hero.subtitle")}</p>
          </div>
          <div className="hero-panel__stats" aria-hidden="true">
            <span>50 MB</span>
            <span>{MIN_EXPIRY_HOURS}-{MAX_EXPIRY_HOURS}h</span>
            <span>{remoteClips.length} clips</span>
          </div>
        </section>

        <section className="card">
          <div className="card__header">
            <div>
              <h2>{t("create.title")}</h2>
              <p className="muted">{t("create.description")}</p>
            </div>
            <div className="card__actions">
              <button
                type="button"
                className="btn btn--primary"
                onClick={handleImportClipboard}
                disabled={isImportingClipboard || type !== "text"}
                title={
                  type === "text"
                    ? t("tooltip.importClipboard")
                    : t("tooltip.switchToText")
                }
              >
                {isImportingClipboard
                  ? t("buttons.importingClipboard")
                  : t("buttons.importClipboard")}
              </button>
            </div>
          </div>

          <div className="remote-form">
            <div className="remote-form__split">
              <div className="stack">
                <div className="field field--horizontal">
                  <span className="field__label">{t("form.contentType")}</span>
                  <div className="pill-group">
                    <button
                      type="button"
                      className={`pill ${type === "text" ? "pill--active" : ""}`}
                      onClick={() => setType("text")}
                    >
                      {t("form.textType")}
                    </button>
                    <button
                      type="button"
                      className={`pill ${type === "file" ? "pill--active" : ""}`}
                      onClick={() => setType("file")}
                    >
                      {t("form.fileType")}
                    </button>
                  </div>
                </div>

                {type === "text" ? (
                  <label className="field">
                    <span className="field__label">{t("form.textLabel")}</span>
                    <textarea
                      value={textContent}
                      onChange={(event) => setTextContent(event.target.value)}
                      placeholder={t("form.textPlaceholder")}
                      rows={6}
                    />
                  </label>
                ) : (
                  <label className="field">
                    <span className="field__label">{t("form.fileLabel")}</span>
                    <input type="file" onChange={handleFileSelect} accept="*" />
                    {selectedFile ? (
                      <div className="file-preview">
                        <span className="file-preview__name">
                          {selectedFile.name}
                        </span>
                        <span className="file-preview__meta">
                          {formatBytes(selectedFile.size)} ·{" "}
                          {selectedFile.type || t("form.unknownType")}
                        </span>
                      </div>
                    ) : (
                      <p className="muted">{t("form.fileSupport")}</p>
                    )}
                  </label>
                )}

                {uploadProgress && type === "file" ? (
                  <div className="upload-progress" role="status" aria-live="polite">
                    <div className="upload-progress__head">
                      <span className="upload-progress__label">
                        {describeUploadStatus(uploadProgress)}
                      </span>
                      <span className="upload-progress__percent">
                        {uploadProgress.totalBytes === 0
                          ? "100%"
                          : `${uploadProgress.percent}%`}
                      </span>
                    </div>
                    <div className="upload-progress__bar" aria-hidden="true">
                      <div
                        className={`upload-progress__fill upload-progress__fill--${uploadProgress.status}`}
                        style={{ width: `${uploadProgress.totalBytes === 0 ? 100 : uploadProgress.percent}%` }}
                      />
                    </div>
                    <div className="upload-progress__foot">
                      <span className="muted small">
                        {t("upload.progressLabel", {
                          uploaded: uploadProgress.uploadedChunks,
                          total: uploadProgress.totalChunks,
                          percent: uploadProgress.percent
                        })}
                        {" · "}
                        {formatBytes(uploadProgress.uploadedBytes)} / {formatBytes(uploadProgress.totalBytes)}
                      </span>
                      {(uploadProgress.status === "uploading" ||
                        uploadProgress.status === "retrying" ||
                        uploadProgress.status === "resuming" ||
                        uploadProgress.status === "initializing" ||
                        uploadProgress.status === "assembling") ? (
                        <button
                          type="button"
                          className="btn btn--tiny btn--danger"
                          onClick={handleCancelUpload}
                          disabled={isCancellingUpload}
                        >
                          {isCancellingUpload
                            ? t("upload.cancelling")
                            : t("buttons.cancelUpload")}
                        </button>
                      ) : null}
                    </div>
                  </div>
                ) : null}

              </div>

              <div className="stack">
                <label className="field">
                  <span className="field__label">{t("form.expiryHoursLabel")}</span>
                  <input
                    type="number"
                    min={MIN_EXPIRY_HOURS}
                    max={MAX_EXPIRY_HOURS}
                    value={expiresInHours}
                    onChange={(event) => setExpiresInHours(Number(event.target.value))}
                  />
                  <span className="field__hint">
                    {t("form.expiryHint", {
                      min: MIN_EXPIRY_HOURS,
                      max: MAX_EXPIRY_HOURS
                    })}
                  </span>
                </label>

                <label className="field">
                  <span className="field__label">{t("form.maxDownloadsLabel")}</span>
                  <input
                    type="number"
                    min={1}
                    max={500}
                    value={maxDownloads}
                    onChange={(event) => {
                      const value = Number(event.target.value);
                      setMaxDownloads(
                        Number.isNaN(value) ? 0 : Math.round(value)
                      );
                    }}
                  />
                  <div className="field__options">
                    {MAX_DOWNLOADS_OPTIONS.map((option) => (
                      <button
                        key={option}
                        type="button"
                        className="btn btn--tiny"
                        onClick={() => setMaxDownloads(option)}
                      >
                        {t("form.maxDownloadsOption", { value: option })}
                      </button>
                    ))}
                  </div>
                  <span className="field__hint">
                    {t("form.maxDownloadsHint", { value: DEFAULT_MAX_DOWNLOADS })}
                  </span>
                </label>

                <fieldset className="field field--group">
                  <legend className="field__label">{t("form.accessCredential")}</legend>
                  <label className="radio">
                    <input
                      type="radio"
                      name="access-mode"
                      value="code"
                      checked={accessMode === "code"}
                      onChange={() => setAccessMode("code")}
                    />
                    <div className="radio__content">
                      <span>{t("form.accessWithCode")}</span>
                      <div className="radio__inline">
                        <strong className="code">{shortCode}</strong>
                        <button
                          type="button"
                          className="btn btn--tiny"
                          onClick={() => setShortCode(generateAccessCode())}
                          disabled={accessMode !== "code"}
                        >
                          {t("buttons.refresh")}
                        </button>
                      </div>
                    </div>
                  </label>

                  <label
                    className={`radio ${settings.persistentToken ? "" : "radio--disabled"}`}
                  >
                    <input
                      type="radio"
                      name="access-mode"
                      value="token"
                      checked={accessMode === "token"}
                      onChange={() => setAccessMode("token")}
                      disabled={!settings.persistentToken}
                    />
                    <div className="radio__content">
                      <span>{t("form.accessWithToken")}</span>
                      {settings.persistentToken ? (
                        <span className="code code--inline">
                          {settings.persistentToken}
                        </span>
                      ) : (
                        <span className="muted small">{t("form.tokenMissing")}</span>
                      )}
                      {settings.persistentToken && tokenExpiryNotice ? (
                        <span className="settings-meta">{tokenExpiryNotice}</span>
                      ) : null}
                    </div>
                  </label>
                </fieldset>

                {isCaptchaEnabled ? (
                  <div className="field">
                    <span className="field__label">{t("form.captchaLabel")}</span>
                    <span className="field__hint">{t("form.captchaHint")}</span>
                    <div className="captcha-box">
                      <Captcha
                        provider={captchaProvider as CaptchaProviderType}
                        siteKey={captchaSiteKey}
                        onTokenChange={(token) => {
                          setCaptchaToken(token ?? "");
                          setCaptchaError(null);
                        }}
                        onError={(message) => {
                          setCaptchaToken("");
                          setCaptchaError(message);
                          setToast({
                            kind: "error",
                            message
                          });
                        }}
                        resetSignal={captchaResetKey}
                        labels={{
                          loading: t("form.captchaLoading"),
                          error: t("form.captchaLoadFailed")
                        }}
                      />
                    </div>
                    {captchaError ? (
                      <span className="field__hint field__hint--error">
                        {captchaError}
                      </span>
                    ) : null}
                  </div>
                ) : null}

                <button
                  type="button"
                  className="btn btn--secondary btn--full"
                  onClick={handleCreateRemoteClip}
                  disabled={isCreatingClip || (isCaptchaEnabled && !captchaToken)}
                >
                  {isCreatingClip ? t("buttons.creating") : t("buttons.create")}
                </button>
              </div>
            </div>
          </div>
        </section>

        <section className="card">
          <div className="card__header">
            <div>
              <h2>{t("list.title")}</h2>
              <p className="muted">{listSummary}</p>
            </div>
          </div>

          <div className="grid">
            {remoteClips.length === 0 ? (
              <div className="empty-state">
                <span className="empty-state__icon">⌘</span>
                <h3>{t("list.summaryEmpty")}</h3>
                <p>{t("create.description")}</p>
              </div>
            ) : null}
            {remoteClips.map((clip) => {
              const consumed = clip.downloadCount >= clip.maxDownloads;
              const expired = clip.expiresAt <= now;
              const inactive = expired || consumed;
              const remainingDownloads = Math.max(
                0,
                clip.maxDownloads - clip.downloadCount
              );
              const hoursLeft = clip.expiresAt - now;
              const badgeTone = inactive
                ? "badge--danger"
                : remainingDownloads <= 2 || hoursLeft <= 60 * 60 * 1000
                ? "badge--warning"
                : "badge--ok";
              const badgeLabel = inactive
                ? consumed
                  ? t("list.badge.limitReached")
                  : t("list.badge.expired")
                : t("list.badge.remaining", {
                    duration: formatRemaining(clip.expiresAt - now)
                  });
              const directAccessUrl = clip.accessCode
                ? clip.directUrl ?? buildRelativeAccessPath(clip.accessCode)
                : "";
              const tokenAccessUrl = clip.accessToken
                ? buildRelativeAccessPath(clip.accessToken)
                : "";
              const clipTypeLabel =
                clip.type === "text"
                  ? t("list.clipType.text")
                  : t("list.clipType.file");
              const clipMeta = t("list.clipMeta", {
                created: formatDateTime(clip.createdAt),
                type: clipTypeLabel
              });
              return (
                <article
                  key={clip.id}
                  className={`remote-card ${inactive ? "remote-card--expired" : ""}`}
                >
                  <header className="remote-card__header">
                    <div>
                      <h4>{getClipTitle(clip)}</h4>
                      <span className="muted small">{clipMeta}</span>
                    </div>
                    <span className={`badge ${badgeTone}`}>{badgeLabel}</span>
                  </header>

                  <div className="remote-card__body">
                    {clip.type === "text" ? (
                      <pre className="remote-card__content">
                        {clip.payload.text ?? ""}
                      </pre>
                    ) : clip.payload.file ? (
                      <div className="file-preview">
                        <span className="file-preview__name">
                          {clip.payload.file.name}
                        </span>
                        <span className="file-preview__meta">
                          {formatBytes(clip.payload.file.size)} ·{" "}
                          {clip.payload.file.type || t("form.unknownType")}
                        </span>
                        <span className="file-preview__meta">
                          {t("list.fileMeta.downloads", {
                            count: clip.downloadCount,
                            max: clip.maxDownloads
                          })}
                        </span>
                      </div>
                    ) : (
                      <span className="muted">{t("list.fileUnavailable")}</span>
                    )}
                  </div>

                  <footer className="remote-card__footer">
                    <div className="remote-card__creds">
                      {clip.accessCode ? (
                        <button
                          type="button"
                          className="badge badge--ghost"
                          onClick={() =>
                            handleCopyAccess(directAccessUrl, "direct-link")
                          }
                        >
                          {t("list.codeLabel", { code: clip.accessCode })}
                        </button>
                      ) : null}
                      {clip.accessToken ? (
                        <button
                          type="button"
                          className="badge badge--ghost"
                          onClick={() =>
                            handleCopyAccess(
                              tokenAccessUrl,
                              "direct-link"
                            )
                          }
                        >
                          {t("list.tokenLabel", { token: clip.accessToken })}
                        </button>
                      ) : null}
                      <span className="muted small">
                        {t("list.remainingDownloads", {
                          count: remainingDownloads
                        })}
                      </span>
                    </div>
                    <div className="remote-card__actions">
                      {clip.type === "text" ? (
                        <button
                          type="button"
                          className="btn btn--tiny"
                          onClick={() => handleCopyRemoteText(clip)}
                          disabled={inactive}
                        >
                          {t("buttons.copyText")}
                        </button>
                      ) : (
                        <button
                          type="button"
                          className="btn btn--tiny"
                          onClick={() => handleDownloadFile(clip)}
                          disabled={inactive}
                        >
                          {t("buttons.downloadFile")}
                        </button>
                      )}
                      <button
                        type="button"
                        className="btn btn--tiny btn--danger"
                        onClick={() => void handleRemoveRemoteClip(clip.id)}
                      >
                        {t("buttons.delete")}
                      </button>
                    </div>
                  </footer>
                </article>
              );
            })}
          </div>
        </section>
      </main>

      {isSettingsOpen ? (
        <div className="modal" role="dialog" aria-modal="true">
          <div className="modal__overlay" onClick={handleCloseSettings} />
          <div className="modal__content">
            <header className="modal__header">
              <div>
                <h3>{t("modal.title")}</h3>
                <p className="muted small">{t("modal.description")}</p>
              </div>
              <button
                type="button"
                className="btn btn--ghost btn--tiny"
                onClick={handleCloseSettings}
              >
                {t("buttons.close")}
              </button>
            </header>

            <div className="stack">
              <label className="field">
                <span className="field__label">{t("modal.tokenLabel")}</span>
                <div className="field field--compact">
                  <input
                    value={settingsTokenDraft}
                    onChange={(event) => setSettingsTokenDraft(event.target.value)}
                    placeholder={t("modal.tokenPlaceholder")}
                  />
                  <button
                    type="button"
                    className="btn btn--tiny"
                    onClick={handleGeneratePersistentToken}
                  >
                    {t("buttons.generateToken")}
                  </button>
                </div>
                <span className="field__hint">
                  {t("modal.tokenHint")}
                </span>
              </label>
              {tokenLastActivityLabel ? (
                <p className="settings-meta">{tokenLastActivityLabel}</p>
              ) : null}
              {settings.persistentToken && tokenExpiryNotice ? (
                <p className="settings-meta">{tokenExpiryNotice}</p>
              ) : null}
            </div>

            <div className="settings-actions settings-actions--modal">
              <button
                type="button"
                className="btn btn--secondary"
                onClick={handleSaveSettings}
              >
                {t("buttons.saveSettings")}
              </button>
            </div>
          </div>
        </div>
      ) : null}

      {toast ? (
        <aside className={`toast toast--${toast.kind}`}>
          <span>{toast.message}</span>
        </aside>
      ) : null}
    </div>
  );
};

export default App;
