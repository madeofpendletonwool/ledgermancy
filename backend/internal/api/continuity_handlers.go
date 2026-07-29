package api

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/continuity"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/jobs"
)

// The continuity surface is owner-only.
//
// This is operator data, not household data: it describes the instance's
// recovery posture, names paths on the host, and can trigger a full database
// dump. A household member has no more business here than they have in the
// compose file. The routes are grouped under auth.RequireOwner in server.go
// rather than checked per handler.

// continuityResponse is the whole panel in one payload.
type continuityResponse struct {
	Enabled  bool               `json:"enabled"`
	Settings continuitySettings `json:"settings"`
	Runs     []continuityRun    `json:"runs"`
	Coverage continuityCoverage `json:"coverage"`
}

type continuitySettings struct {
	Dir                 string `json:"dir"`
	MirrorDir           string `json:"mirror_dir"`
	Interval            string `json:"interval"`
	RestoreTestInterval string `json:"restore_test_interval"`
	IncludeDocuments    bool   `json:"include_documents"`
	KeepDaily           int    `json:"keep_daily"`
	KeepWeekly          int    `json:"keep_weekly"`
	KeepMonthly         int    `json:"keep_monthly"`
}

// continuityRun is one line of the panel.
type continuityRun struct {
	Kind   string `json:"kind"`
	Health string `json:"health"`
	Status string `json:"status,omitempty"`
	// At is the run's start; Age is how long ago, phrased.
	At  *time.Time `json:"at,omitempty"`
	Age string     `json:"age,omitempty"`
	// Headline states the consequence, not the status. See panelHeadline.
	Headline     string `json:"headline"`
	Detail       string `json:"detail,omitempty"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
	ArtifactPath string `json:"artifact_path,omitempty"`
}

// continuityCoverage is what the panel says about the registry: how many tables
// are in each class, and what durable state lives outside Postgres.
type continuityCoverage struct {
	InExport   int                  `json:"in_export"`
	DumpOnly   int                  `json:"dump_only"`
	Derived    int                  `json:"derived"`
	Ephemeral  int                  `json:"ephemeral"`
	BlobStores []continuityBlobInfo `json:"blob_stores"`
}

type continuityBlobInfo struct {
	Name string `json:"name"`
	Why  string `json:"why"`
}

// panelKinds is the order the panel lists them, most consequential first.
//
// The restore test sits above the backup deliberately. An operator scanning
// this page should meet "has this been verified" before "has this run", because
// the second question is the one they already assume they know the answer to.
var panelKinds = []string{
	continuity.KindRestoreTest,
	continuity.KindDBDump,
	continuity.KindDocumentsArchive,
	continuity.KindExport,
	continuity.KindMirrorPush,
	continuity.KindKeyAck,
}

func (s *Server) handleContinuityStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config.Backup

	latest, err := s.Queries.LatestBackupRunPerKind(r.Context())
	if err != nil {
		s.internalError(w, "latest backup runs", err)
		return
	}
	byKind := map[string]int{}
	for i, run := range latest {
		byKind[run.Kind] = i
	}

	now := time.Now()
	resp := continuityResponse{
		Enabled: cfg.Enabled,
		Settings: continuitySettings{
			Dir:                 cfg.Dir,
			MirrorDir:           cfg.MirrorDir,
			Interval:            cfg.Interval.String(),
			RestoreTestInterval: cfg.RestoreTestInterval.String(),
			IncludeDocuments:    cfg.IncludeDocuments,
			KeepDaily:           cfg.KeepDaily,
			KeepWeekly:          cfg.KeepWeekly,
			KeepMonthly:         cfg.KeepMonthly,
		},
		Coverage: continuityCoverage{
			InExport:  len(continuity.TablesWithCoverage(continuity.InExport)),
			DumpOnly:  len(continuity.TablesWithCoverage(continuity.DumpOnly)),
			Derived:   len(continuity.TablesWithCoverage(continuity.Derived)),
			Ephemeral: len(continuity.TablesWithCoverage(continuity.Ephemeral)),
		},
	}
	for _, store := range continuity.BlobStores() {
		resp.Coverage.BlobStores = append(resp.Coverage.BlobStores,
			continuityBlobInfo{Name: store.Name, Why: store.Why})
	}

	for _, kind := range panelKinds {
		line := continuityRun{Kind: kind}

		switch {
		case !cfg.Enabled && kind != continuity.KindKeyAck:
			// The key acknowledgement is still worth asking about with backups
			// off: an operator who has switched them off deliberately still
			// cannot restore anything without the key.
			line.Health = string(continuity.HealthOff)
		case kind == continuity.KindDocumentsArchive && !cfg.IncludeDocuments:
			line.Health = string(continuity.HealthOff)
		case kind == continuity.KindMirrorPush && cfg.MirrorDir == "":
			line.Health = string(continuity.HealthOff)
		}

		if idx, ok := byKind[kind]; ok && line.Health == "" {
			run := latest[idx]
			at := run.StartedAt
			line.Status = run.Status
			line.At = &at
			line.Age = continuity.Age(now.Sub(at))
			line.Health = string(continuity.ClassifyRun(kind, run.Status, at, now))
			if run.Detail != nil {
				line.Detail = *run.Detail
			}
			if run.SizeBytes != nil {
				line.SizeBytes = *run.SizeBytes
			}
			if run.ArtifactPath != nil {
				line.ArtifactPath = *run.ArtifactPath
			}
		} else if line.Health == "" {
			line.Health = string(continuity.HealthNeverRun)
		}

		line.Headline = panelHeadline(kind, continuity.Health(line.Health), line.At, now)
		resp.Runs = append(resp.Runs, line)
	}

	writeJSON(w, http.StatusOK, resp)
}

// panelHeadline writes the sentence the operator actually reads.
//
// Not "backup status: stale". Operators under-invest in continuity because
// nothing ever states the stakes plainly — a status word is something to
// acknowledge and move past, whereas a consequence is something to act on. Each
// string below names what would happen if the failure it describes were tested
// today, in the second person, without hedging.
func panelHeadline(kind string, health continuity.Health, at *time.Time, now time.Time) string {
	days := 0
	if at != nil {
		days = int(now.Sub(*at).Hours() / 24)
	}

	switch kind {
	case continuity.KindRestoreTest:
		// Every string here says the app did it, or failed to.
		//
		// An earlier draft said "your last verified restore was N days ago",
		// which reads as a chore the operator owes — nobody wants to restore
		// their live instance weekly, and nobody should. The check runs by
		// itself, against a temporary copy, and the live database is never
		// touched. Copy that leaves that ambiguous invites the reader either to
		// do something unnecessary and risky, or to ignore the row entirely.
		switch health {
		case continuity.HealthOff:
			return "Backups are switched off, so nothing is being verified."
		case continuity.HealthNeverRun:
			return "No restore test has run yet, so nothing has confirmed these backups can be " +
				"restored at all. The check runs on its own and never touches your live database."
		case continuity.HealthBad:
			if days > 0 {
				return fmt.Sprintf("The automatic restore check has not succeeded in %d days. "+
					"Nothing has confirmed your backups still restore, so if the database failed "+
					"today you would find out then.", days)
			}
			return "The last automatic restore check failed: your backups did not restore cleanly. " +
				"Read the detail below — this is a problem now, not during an outage."
		case continuity.HealthWarn:
			return fmt.Sprintf("Last checked %d days ago. Still fine — this is the check that turns "+
				"a backup into a recovery, so it is worth keeping green.", days)
		}
		return "Your latest backup was restored into a temporary database and verified, row counts " +
			"and all. Your live database was not touched. This is the line that matters."

	case continuity.KindDBDump:
		switch health {
		case continuity.HealthOff:
			return "No database backups are being taken. A disk failure would be final."
		case continuity.HealthNeverRun:
			return "No backup has been taken yet."
		case continuity.HealthBad:
			if days > 0 {
				return fmt.Sprintf("No successful backup for %d days. Everything since then would be lost, "+
					"and Plaid cannot re-supply your net-worth history at any price.", days)
			}
			return "The last backup failed. You are running without a current one."
		case continuity.HealthWarn:
			return fmt.Sprintf("Last backup %d days ago — older than it should be.", days)
		}
		return "The database has a current backup."

	case continuity.KindDocumentsArchive:
		switch health {
		case continuity.HealthOff:
			return "Document contents are not being backed up. A restore would list every document and open none of them."
		case continuity.HealthNeverRun:
			return "The document vault has not been archived yet."
		case continuity.HealthBad, continuity.HealthWarn:
			return fmt.Sprintf("Documents last archived %d days ago. Anything uploaded since exists only on this host.", days)
		}
		return "Document contents are archived alongside the database."

	case continuity.KindExport:
		switch health {
		case continuity.HealthOff:
			return "No portable export is being written."
		case continuity.HealthNeverRun:
			return "No portable export yet. The database dump restores this app; the export outlives it."
		case continuity.HealthBad, continuity.HealthWarn:
			return fmt.Sprintf("Last export %d days ago.", days)
		}
		return "A portable, plain-JSON copy of your data exists and is current."

	case continuity.KindMirrorPush:
		switch health {
		case continuity.HealthOff:
			// Deliberately does not claim the backups are on this host.
			//
			// BACKUP_DIR is a path, and a path says nothing about the hardware
			// underneath it — a bind mount of a NAS share is already off-host
			// and is a perfectly good primary destination. The app cannot tell
			// the difference and must not pretend to. What it does know is that
			// only one destination is configured, so that is all it says.
			return "Only one backup location is configured. If BACKUP_DIR already points at " +
				"storage on another machine, you may well be covered — the app cannot see what " +
				"is underneath that path. If it is a disk in this machine, a hardware failure " +
				"takes the backups with it. Set BACKUP_MIRROR_DIR for a second copy either way."
		case continuity.HealthNeverRun:
			return "A second location is configured but nothing has been copied to it yet."
		case continuity.HealthBad, continuity.HealthWarn:
			return fmt.Sprintf("The second copy was last updated %d days ago and is behind the primary.", days)
		}
		return "Backups are being written to two separate locations."

	case continuity.KindKeyAck:
		if health == continuity.HealthNeverRun {
			return "You have not confirmed that ENCRYPTION_KEY is stored somewhere safe. " +
				"Without it a restored database cannot decrypt its own Plaid tokens or a single document — " +
				"the backup would be intact and unusable. No one can recover it for you."
		}
		// Phrased with Age rather than a day count: this row is the one an
		// operator can turn green in a single click, so it routinely reads
		// moments after the fact, and "0 days ago" is how a page loses the
		// reader's trust in everything else it says.
		when := "at some point"
		if at != nil {
			when = continuity.Age(now.Sub(*at))
		}
		return fmt.Sprintf("You confirmed your ENCRYPTION_KEY was stored safely %s. "+
			"Check it is still where you left it.", when)
	}
	return ""
}

// handleContinuityKeyAck records that the operator has stored ENCRYPTION_KEY.
//
// The app cannot verify this and does not pretend to — see
// jobs.RecordKeyAcknowledgement. Asking is the point.
func (s *Server) handleContinuityKeyAck(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	if err := jobs.RecordKeyAcknowledgement(r.Context(), s.Queries, identity.Email); err != nil {
		s.internalError(w, "record key acknowledgement", err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

type continuityRunRequest struct {
	Kind string `json:"kind"`
}

// handleContinuityRun kicks a backup or restore test now.
//
// Returns the enqueue error rather than swallowing it, unlike the app's
// fire-and-forget enqueues: the operator is standing in front of a button and
// needs to know whether it took.
func (s *Server) handleContinuityRun(w http.ResponseWriter, r *http.Request) {
	var req continuityRunRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.Config.Backup.Enabled {
		writeError(w, http.StatusConflict,
			"backups are switched off (BACKUP_ENABLED=false), so there is nothing to run")
		return
	}

	var err error
	switch req.Kind {
	case "backup", continuity.KindDBDump:
		err = jobs.EnqueueBackup(r.Context(), s.Jobs)
	case continuity.KindRestoreTest:
		err = jobs.EnqueueRestoreTest(r.Context(), s.Jobs)
	default:
		writeError(w, http.StatusBadRequest, `kind must be "backup" or "restore_test"`)
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// handleContinuityExport streams a portable export on demand, through the same
// exporter the scheduled job uses.
//
// One code path, so the download can never quietly diverge from the artefact on
// disk — and so the coverage guard covers both at once.
//
// An instance hosts exactly one household by construction: the first user
// bootstraps it and every registration after that is invite-only into it (see
// handleRegister). So the operator export and the household export are the same
// thing, and no per-table household filter is needed. That is a property of the
// current data model rather than a law, so it is asserted below instead of
// assumed — if a future wave makes an instance multi-household, this refuses
// rather than handing one household another's ledger.
func (s *Server) handleContinuityExport(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	households, err := s.Queries.ListHouseholdIDs(r.Context())
	if err != nil {
		s.internalError(w, "list households", err)
		return
	}
	if len(households) != 1 || households[0] != identity.HouseholdID {
		writeError(w, http.StatusConflict,
			"this instance hosts more than one household, so a whole-instance export is not "+
				"yours to download. Take the export from the backup directory on the host instead.")
		return
	}

	export, err := (&continuity.Exporter{Pool: s.Pool}).Build(r.Context())
	if err != nil {
		s.internalError(w, "build export", err)
		return
	}

	filename := fmt.Sprintf("ledgermancy-export-%s.json.gz", time.Now().Format("2006-01-02"))
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	gz := gzip.NewWriter(w)
	defer gz.Close()
	enc := json.NewEncoder(gz)
	enc.SetIndent("", "  ")
	if err := enc.Encode(export); err != nil {
		// The status line and part of the body are already on the wire, so the
		// download will truncate. Logging is all that is left — and a truncated
		// .gz fails to decompress rather than decoding as a short export, which
		// is the failure mode we want.
		slog.Error("write export download", "error", err)
	}
}
