package schemas

import "fmt"

// This file is the single source of truth for the upper bound of every
// client-supplied parameter.
//
// Why it exists
// -------------
// Before these bounds the JSON body was only size-limited by readBody's
// MaxBytesReader (1 MiB for the clip routes, more for chunk uploads). Every
// string field was therefore "as large as the body", which is an amplification
// vector: a single request could pin megabytes in Go heap, in the JSON decoder,
// in the database row and in every list response that echoes the clip back.
// The worst offenders were `payload.text` and `accessToken` (both declared with
// max_length = 0, i.e. unbounded) and the clip `token`, which is also used as a
// persistent lookup key.
//
// Two numbers matter for a string:
//   - characters (runes), mirroring pydantic's max_length;
//   - UTF-8 bytes, which is what an attacker can actually inflate (4 bytes per
//     character) and what the process/memory/DB actually pay for.
//
// Both are enforced, so "64 KiB of text" means 64 KiB no matter the encoding.
// Text larger than that is not rejected silently: the hint tells the client to
// upload it as a file clip (chunked upload), which is the designed path for
// bulk content.
const (
	// MaxClipTextBytes caps payload.text for text clips: 64 KiB.
	MaxClipTextBytes = 64 * 1024
	// MaxClipTextRunes caps payload.text by character count. A well behaved
	// client never hits it for ASCII text; it only stops multibyte text from
	// being billed as "few characters" while costing many times the bytes.
	MaxClipTextRunes = 64 * 1024

	// MaxAccessTokenRunes/MaxAccessTokenBytes cap the persistent token used by
	// both POST /api/clips and POST /api/uploads/init, and the token of
	// POST /api/tokens/register (the same value, so the same bound).
	MaxAccessTokenRunes = 128
	MaxAccessTokenBytes = 128
	// MinAccessTokenRunes keeps the historical `min_length=7`.
	MinAccessTokenRunes = 7

	// MaxEnvironmentIDRunes/MaxEnvironmentIDBytes cap environmentId. It is part
	// of every index and of most query strings, so it stays short.
	MaxEnvironmentIDRunes = 64
	MaxEnvironmentIDBytes = 64
	// MinEnvironmentIDRunes keeps the historical `min_length=1`.
	MinEnvironmentIDRunes = 1

	// MaxAccessCodeRunes/MinAccessCodeRunes keep `accessCode: min_length=5,
	// max_length=12`; the alphanumeric validator already restricts the alphabet.
	MaxAccessCodeRunes = 12
	MinAccessCodeRunes = 5

	// MaxCaptchaTokenRunes/MaxCaptchaTokenBytes keep the historical
	// `max_length=4096`. The token is forwarded upstream, never stored.
	MaxCaptchaTokenRunes = 4096
	MaxCaptchaTokenBytes = 4096

	// MaxStoredFileNameRunes/MaxStoredFileNameBytes keep `name:
	// max_length=255` and stop a multibyte filename from overflowing it.
	MaxStoredFileNameRunes = 255
	MaxStoredFileNameBytes = 255

	// MaxMimeTypeRunes/MaxMimeTypeBytes keep `type: max_length=255` (and the
	// identical mimeType bound of the upload init request).
	MaxMimeTypeRunes = 255
	MaxMimeTypeBytes = 255

	// MaxDataURLBytes caps the embedded base64 data URI of an inline file clip.
	// This route is additionally bounded by readBody, so the value is a hard
	// ceiling for the field itself; anything bigger belongs on the chunked
	// upload path (POST /api/uploads/init + PUT .../chunks/{index}).
	MaxDataURLBytes = 8 * 1024 * 1024

	// MinRequestIDRunes/MaxRequestIDRunes/... keep the idempotency key of
	// POST /api/uploads/init bounded (`min_length=4, max_length=128`).
	MinRequestIDRunes = 4
	MaxRequestIDRunes = 128
	MaxRequestIDBytes = 128
)

// clipTextBound is the only place payload.text is sized. MinRunes stays 0 on
// purpose: the model validator already rejects empty/whitespace-only text with
// the localized "文本片段需要 text 字段" message.
var clipTextBound = stringBound{
	MaxRunes: MaxClipTextRunes,
	MaxBytes: MaxClipTextBytes,
	// A byte-bound failure is a size-limit failure, so it keeps the
	// `string_too_long` error type the client already maps, but the message
	// explains the remedy instead of just stating a number.
	TooLongMessage: clipTextTooLongMessage(),
}

// accessTokenBound bounds every persistent token (clip create, upload init and
// token register) with the historical `min_length=7`.
var accessTokenBound = stringBound{
	MinRunes: MinAccessTokenRunes,
	MaxRunes: MaxAccessTokenRunes,
	MaxBytes: MaxAccessTokenBytes,
}

// environmentIDBound bounds environmentId (required variant).
var environmentIDBound = stringBound{
	MinRunes: MinEnvironmentIDRunes,
	MaxRunes: MaxEnvironmentIDRunes,
	MaxBytes: MaxEnvironmentIDBytes,
}

// accessCodeBound bounds accessCode (optional variant).
var accessCodeBound = stringBound{
	MinRunes: MinAccessCodeRunes,
	MaxRunes: MaxAccessCodeRunes,
}

// captchaTokenBound bounds captchaToken.
var captchaTokenBound = stringBound{
	MinRunes: 1,
	MaxRunes: MaxCaptchaTokenRunes,
	MaxBytes: MaxCaptchaTokenBytes,
}

// storedFileNameBound bounds the uploaded file name.
var storedFileNameBound = stringBound{
	MinRunes: 1,
	MaxRunes: MaxStoredFileNameRunes,
	MaxBytes: MaxStoredFileNameBytes,
}

// mimeTypeBound bounds both `payload.file.type` and the upload init `mimeType`.
var mimeTypeBound = stringBound{
	MaxRunes: MaxMimeTypeRunes,
	MaxBytes: MaxMimeTypeBytes,
}

// dataURLBound bounds the inline base64 payload of a file clip.
var dataURLBound = stringBound{
	MinRunes: 1,
	MaxBytes: MaxDataURLBytes,
	// The historical min_length=1 message is kept by MinRunes; the byte message
	// points the client at the chunked upload endpoint.
	TooLongMessage: "file dataUrl 过大，请改用分片上传（POST /api/uploads/init）",
}

// requestIDBound bounds the idempotency key of POST /api/uploads/init.
var requestIDBound = stringBound{
	MinRunes: MinRequestIDRunes,
	MaxRunes: MaxRequestIDRunes,
	MaxBytes: MaxRequestIDBytes,
}

// RequestIDBound exposes the idempotency-key bound for documentation/tests.
var RequestIDBound = requestIDBound

// ClipTextBound exposes the payload.text bound for documentation/tests.
var ClipTextBound = clipTextBound

// AccessTokenBound exposes the persistent-token bound for documentation/tests.
var AccessTokenBound = accessTokenBound

// clipTextTooLongMessage spells out the bound and the way out: oversized text
// belongs on the chunked file upload path instead of the inline text clip.
func clipTextTooLongMessage() string {
	return fmt.Sprintf("文本片段最长 %d 字节（%d KiB，%d 字符），更长的内容请作为文件片段上传",
		MaxClipTextBytes, MaxClipTextBytes/1024, MaxClipTextRunes)
}
