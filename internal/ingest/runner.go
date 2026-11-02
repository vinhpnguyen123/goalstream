package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/audit"
	"github.com/vinhpnguyen123/goalstream/internal/config"
	"github.com/vinhpnguyen123/goalstream/internal/recommend"
	"github.com/vinhpnguyen123/goalstream/internal/storage"
	"github.com/vinhpnguyen123/goalstream/internal/transcode"
	"github.com/vinhpnguyen123/goalstream/internal/vocab"
)

type Runner struct {
	Pool  *pgxpool.Pool
	Store *storage.Client
	Cfg   config.IngestConfig
	Log   *slog.Logger
}

// Summary is printed at the end of a run. Cuts counts ffmpeg cut invocations: a re-run must
// cut nothing that is already ingested (only previously rejected events are re-judged).
type Summary struct {
	Matches  int
	Ingested int
	Present  int
	Cuts     int
	Rejected map[string]int
}

// Run ingests every match under dataDir/matches/<league>/<season>/<match>/.
// Per-event problems become audited rejections; only infrastructure failures (database,
// storage, a full vocab) abort the run.
func (r *Runner) Run(ctx context.Context, dataDir string) (Summary, error) {
	sum := Summary{Rejected: map[string]int{}}
	root := filepath.Join(dataDir, "matches")
	matches, err := matchDirs(root)
	if err != nil {
		return sum, err
	}
	tmp, err := os.MkdirTemp("", "goalstream-ingest-*")
	if err != nil {
		return sum, fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmp) // also on Ctrl-C: the context kills ffmpeg, then this runs

	for _, rel := range matches {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		sum.Matches++
		if err := r.ingestMatch(ctx, root, rel, tmp, &sum); err != nil {
			return sum, fmt.Errorf("match %s: %w", rel, err)
		}
	}
	return sum, nil
}

// matchDirs lists <league>/<season>/<match> paths, skipping anything starting with "." at
// every level (hf_hub_download leaves .cache/huggingface/ in the tree), sorted for stable runs.
func matchDirs(root string) ([]string, error) {
	var out []string
	var walk func(rel string, depth int) error
	walk = func(rel string, depth int) error {
		entries, err := os.ReadDir(filepath.Join(root, rel))
		if err != nil {
			return fmt.Errorf("reading %s: %w", filepath.Join(root, rel), err)
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			child := filepath.Join(rel, e.Name())
			if depth == 2 {
				out = append(out, child)
			} else if err := walk(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk("", 0); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// half caches one half file's existence and probe for the whole match.
type half struct {
	path   string
	exists bool
	probe  transcode.Probe
	err    error
}

func (r *Runner) ingestMatch(ctx context.Context, root, rel, tmp string, sum *Summary) error {
	dir := filepath.Join(root, rel)
	parts := strings.Split(rel, string(filepath.Separator)) // league, season, folder
	competition, season, folder := parts[0], parts[1], parts[2]

	f, err := os.Open(filepath.Join(dir, "Labels-v2.json"))
	if errors.Is(err, os.ErrNotExist) {
		return r.rejectMatch(ctx, rel, ReasonMissingLabels, sum)
	} else if err != nil {
		return fmt.Errorf("opening labels: %w", err)
	}
	labels, err := ParseLabels(f)
	f.Close()
	if err != nil {
		r.Log.Warn("labels unreadable", "match", rel, "err", err)
		return r.rejectMatch(ctx, rel, ReasonBadMatchMetadata, sum)
	}
	meta, err := labels.Meta()
	if err != nil {
		r.Log.Warn("match metadata invalid", "match", rel, "err", err)
		return r.rejectMatch(ctx, rel, ReasonBadMatchMetadata, sum)
	}
	if !FolderMatches(folder, meta.Home, meta.Away) {
		r.Log.Warn("folder name disagrees with JSON teams; using JSON", "match", rel, "home", meta.Home, "away", meta.Away)
	}
	if u := strings.TrimSuffix(labels.UrlLocal, "/"); u != "" && u != filepath.ToSlash(rel) {
		r.Log.Warn("UrlLocal disagrees with the filesystem path; using the path", "match", rel, "url_local", labels.UrlLocal)
	}

	var matchID string
	err = r.Pool.QueryRow(ctx, `
		INSERT INTO matches (soccernet_path, home_team, away_team, competition, season, match_date)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (soccernet_path) DO UPDATE SET soccernet_path = EXCLUDED.soccernet_path
		RETURNING id`, filepath.ToSlash(rel), meta.Home, meta.Away, competition, season, meta.Date).Scan(&matchID)
	if err != nil {
		return fmt.Errorf("upserting match: %w", err)
	}

	wanted := map[string]bool{}
	for _, l := range r.Cfg.Labels {
		wanted[l] = true
	}
	halves := map[int]*half{}
	m := matchCtx{id: matchID, rel: rel, dir: dir, competition: competition, season: season, meta: meta, halves: halves}
	for _, a := range labels.Annotations {
		if !wanted[a.Label] {
			continue // out of scope, not a rejection: auditing every throw-in would bury real ones
		}
		if err := r.ingestEvent(ctx, m, a, tmp, sum); err != nil {
			return err
		}
	}
	return nil
}

type matchCtx struct {
	id, rel, dir        string
	competition, season string
	meta                MatchMeta
	halves              map[int]*half
}

func (r *Runner) ingestEvent(ctx context.Context, m matchCtx, a Annotation, tmp string, sum *Summary) error {
	h, pos, parseErr := ParseEvent(a)
	reject := func(reason string) error { return r.rejectClip(ctx, m, a, h, pos, reason, sum) }

	if a.Visibility == "not shown" {
		return reject(ReasonNotShown) // happened off-camera or during a replay: 60 s of the wrong thing
	}
	if parseErr != nil {
		r.Log.Warn("bad annotation", "match", m.rel, "err", parseErr)
		return reject(ReasonBadAnnotation)
	}

	// Dedup pre-check BEFORE any ffmpeg work: this is what makes a re-run cheap.
	var exists bool
	if err := r.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM clips WHERE match_id=$1 AND half=$2 AND position_ms=$3 AND event_type=$4)`,
		m.id, h, pos, a.Label).Scan(&exists); err != nil {
		return fmt.Errorf("dedup check: %w", err)
	}
	if exists {
		sum.Present++
		return nil
	}

	src := r.halfFile(ctx, m, h)
	if !src.exists {
		return reject(ReasonMissingVideo)
	}
	if src.err != nil {
		r.Log.Warn("source half unreadable", "file", src.path, "err", src.err)
		return reject(ReasonCutFailed)
	}
	w, err := CutWindow(pos, src.probe.DurationMS, int(r.Cfg.PreRoll.Milliseconds()), int(r.Cfg.PostRoll.Milliseconds()))
	if err != nil {
		return reject(ReasonPositionOutOfRange)
	}

	out := filepath.Join(tmp, "cut.mp4")
	defer os.Remove(out)
	sum.Cuts++
	capBps := MezzanineCap(src.probe.FormatBps, r.Cfg.MaxRate)
	if err := transcode.CutMezzanine(ctx, src.path, out, w.StartMS, w.DurationMS, r.Cfg.CRF, capBps, r.Cfg.CutTimeout); err != nil {
		if ctx.Err() != nil {
			return ctx.Err() // interrupted, not a media problem
		}
		r.Log.Warn("cut failed", "match", m.rel, "half", h, "position_ms", pos, "err", err)
		return reject(ReasonCutFailed)
	}

	p, err := transcode.ProbeFile(ctx, out)
	if err != nil {
		r.Log.Warn("probe of cut failed", "match", m.rel, "err", err)
		return reject(ReasonDecodeFailed)
	}
	if reason := QC(p, int(r.Cfg.MinClip.Milliseconds()), transcode.DecodeCheck(ctx, out)); reason != "" {
		return reject(reason)
	}

	return r.publish(ctx, m, a, h, pos, out, p, sum)
}

// halfFile returns the probed half, probing each file once per match.
func (r *Runner) halfFile(ctx context.Context, m matchCtx, h int) *half {
	if hf, ok := m.halves[h]; ok {
		return hf
	}
	hf := &half{path: filepath.Join(m.dir, fmt.Sprintf("%d_720p.mkv", h))}
	if _, err := os.Stat(hf.path); err == nil {
		hf.exists = true
		hf.probe, hf.err = transcode.ProbeFile(ctx, hf.path)
	}
	m.halves[h] = hf
	return hf
}

// publish resolves vocab, builds the embedding, uploads the mezzanine, then inserts the row.
// ORDER MATTERS: the bucket is physical truth, so the object is written BEFORE the row. A row
// pointing at a missing object is a broken clip; an object with no row is inert garbage that
// a reconciliation sweep can find.
func (r *Runner) publish(ctx context.Context, m matchCtx, a Annotation, h, pos int, file string, p transcode.Probe, sum *Summary) error {
	var scoring *string
	var isHome *bool
	switch a.Team {
	case "home":
		scoring, isHome = &m.meta.Home, ptr(true)
	case "away":
		scoring, isHome = &m.meta.Away, ptr(false)
	} // "not applicable" → both NULL

	var feat recommend.Features
	err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		var err error
		if scoring != nil {
			s, err := vocab.Slot(ctx, tx, "team", *scoring)
			if err != nil {
				return err
			}
			feat.TeamSlot = &s
		}
		if feat.CompSlot, err = vocab.Slot(ctx, tx, "competition", m.competition); err != nil {
			return err
		}
		feat.SeasonSlot, err = vocab.Slot(ctx, tx, "season", m.season)
		return err
	})
	if err != nil {
		return fmt.Errorf("resolving vocab: %w", err) // includes vocab.ErrFull: abort the run, loudly
	}
	minute := MatchMinute(h, pos)
	feat.MatchMinute, feat.IsHome = minute, isHome // tags are empty until hand-labelled
	vec, err := recommend.Embed(feat)
	if err != nil {
		return fmt.Errorf("embedding: %w", err)
	}

	id := uuid.NewString()
	prefix := "clips/" + id + "/"
	if err := r.Store.PutFile(ctx, prefix+"original.mp4", file, "video/mp4", ""); err != nil {
		return err // storage down: abort the run rather than reject a good clip
	}

	err = pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		var inserted string
		err := tx.QueryRow(ctx, `
			INSERT INTO clips (id, match_id, half, position_ms, event_type, title, scoring_team, is_home,
			                   match_minute, s3_prefix, width, height, source_bitrate, duration_ms, embedding)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15::vector)
			ON CONFLICT ON CONSTRAINT clips_dedup_uniq DO NOTHING
			RETURNING id`,
			id, m.id, h, pos, a.Label, Title(m.meta.Home, m.meta.Away, a.Label, h, minute), scoring, isHome,
			minute, prefix, p.Width, p.Height, p.VideoBps, p.DurationMS, recommend.Literal(vec)).Scan(&inserted)
		if errors.Is(err, pgx.ErrNoRows) {
			return errLostRace
		}
		if err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Entry{
			EntityType: "clip", EntityID: id, Action: "clip_ingested", ActorType: "system",
			Metadata: map[string]any{"source": "ingest", "event_type": a.Label, "duration_ms": p.DurationMS,
				"source_bitrate": p.VideoBps, "width": p.Width, "height": p.Height},
		})
	})
	if errors.Is(err, errLostRace) {
		// Another ingest inserted the same event between our pre-check and now. Our object is
		// an orphan: logged, never deleted (deleting media on a heuristic needs a human).
		r.Log.Warn("lost dedup race; uploaded object is an orphan", "key", prefix+"original.mp4")
		sum.Present++
		return nil
	}
	if err != nil {
		return fmt.Errorf("inserting clip: %w", err)
	}
	sum.Ingested++
	r.Log.Info("ingested", "clip", id, "match", m.rel, "half", h, "minute", minute)
	return nil
}

var errLostRace = errors.New("dedup race lost")

func ptr[T any](v T) *T { return &v }

// rejectClip audits a rejected event against its MATCH: a rejected clip has no row to point at.
func (r *Runner) rejectClip(ctx context.Context, m matchCtx, a Annotation, h, pos int, reason string, sum *Summary) error {
	sum.Rejected[reason]++
	meta := map[string]any{"source": "ingest", "reason": reason, "label": a.Label, "game_time": a.GameTime}
	if h != 0 {
		meta["half"], meta["position_ms"] = h, pos
	}
	return audit.Write(ctx, r.Pool, audit.Entry{
		EntityType: "match", EntityID: m.id, Action: "clip_rejected", ActorType: "system", Metadata: meta,
	})
}

// rejectMatch audits a match that has no row (bad or missing labels). Its entity id is
// derived from the path (md5 → uuid) so every run audits the same match under the same id.
func (r *Runner) rejectMatch(ctx context.Context, rel, reason string, sum *Summary) error {
	sum.Rejected[reason]++
	_, err := r.Pool.Exec(ctx, `
		INSERT INTO audit_log (entity_type, entity_id, action, actor_type, metadata)
		VALUES ('match', md5($1)::uuid, 'match_rejected', 'system', jsonb_build_object('source','ingest','reason',$2::text,'path',$1::text))`,
		filepath.ToSlash(rel), reason)
	if err != nil {
		return fmt.Errorf("audit match_rejected: %w", err)
	}
	return nil
}
