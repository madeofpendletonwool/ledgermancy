package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/ratelimit"
)

// Auth event types. These are the answer to "was that login me?", so the set
// covers every way an account's access can change, not just logins.
const (
	eventLoginSucceeded  = "login_succeeded"
	eventLoginFailed     = "login_failed"
	eventLoginLocked     = "login_locked"
	eventLogout          = "logout"
	eventRegistered      = "registered"
	eventMFAChallenged   = "mfa_challenged"
	eventMFASucceeded    = "mfa_succeeded"
	eventMFAFailed       = "mfa_failed"
	eventMFAEnabled      = "mfa_enabled"
	eventMFADisabled     = "mfa_disabled"
	eventRecoveryUsed    = "recovery_code_used"
	eventRecoveryRotated = "recovery_codes_regenerated"
	eventPasswordChanged = "password_changed"
	eventSessionRevoked  = "session_revoked"
	eventInviteCreated   = "invite_created"
	eventInviteAccepted  = "invite_accepted"
)

// audit records a security-relevant event.
//
// It is best effort by design: a failed insert is logged and swallowed rather
// than failing the request. The same judgement is already made for
// MarkInviteAccepted in handleRegister — refusing a legitimate login because
// an audit row would not write trades a real outage for a bookkeeping gap.
//
// userID may be uuid.Nil for a failed login against an address that does not
// resolve to a user; emailAttempted carries the address in that case so the
// log still shows what is being probed.
func (s *Server) audit(
	ctx context.Context,
	r *http.Request,
	userID uuid.UUID,
	emailAttempted string,
	eventType string,
	metadata map[string]any,
) {
	// Detach from the request context. Several call sites audit an outcome as
	// they respond, and a client that has already disconnected would otherwise
	// cancel the insert — losing precisely the records of abandoned or
	// scripted attempts that are most worth keeping.
	ctx = context.WithoutCancel(ctx)

	var userPtr *uuid.UUID
	if userID != uuid.Nil {
		userPtr = &userID
	}

	var emailPtr *string
	if emailAttempted != "" {
		emailPtr = &emailAttempted
	}

	payload := []byte("{}")
	if len(metadata) > 0 {
		encoded, err := json.Marshal(metadata)
		if err != nil {
			slog.Error("encode audit metadata", "error", err, "event", eventType)
		} else {
			payload = encoded
		}
	}

	var clientIP, userAgent *string
	if ip := ratelimit.ClientIP(r); ip != "" {
		clientIP = &ip
	}
	if ua := r.UserAgent(); ua != "" {
		// Cap it: User-Agent is attacker-controlled and unbounded, and there is
		// no value in storing a megabyte of it.
		if len(ua) > 512 {
			ua = ua[:512]
		}
		userAgent = &ua
	}

	if err := s.Queries.RecordAuthEvent(ctx, dbgen.RecordAuthEventParams{
		UserID:         userPtr,
		EmailAttempted: emailPtr,
		EventType:      eventType,
		ClientIp:       clientIP,
		UserAgent:      userAgent,
		Metadata:       payload,
	}); err != nil {
		slog.Error("record auth event", "error", err, "event", eventType)
	}
}
