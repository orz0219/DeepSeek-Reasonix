package sessioncatalog

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type pageCursor struct {
	Pinned   int    `json:"p"`
	Activity int64  `json:"a"`
	TopicID  string `json:"t"`
}

func (c *Catalog) ListTopics(ctx context.Context, req TopicPageRequest) (TopicPage, error) {
	out := TopicPage{Items: []TopicRecord{}, Revision: c.revision.Load()}
	req.Scope, req.WorkspaceRoot = normalizeScope(req.Scope, req.WorkspaceRoot)
	if req.Limit <= 0 {
		req.Limit = DefaultLimit
	}
	if req.Limit > MaxLimit {
		req.Limit = MaxLimit
	}
	cursor, err := decodeCursor(req.Cursor)
	if err != nil {
		return out, err
	}
	args := []any{req.Scope, req.WorkspaceRoot}
	where := `scope=? AND workspace_root=?`
	if query := strings.TrimSpace(req.Query); query != "" {
		where += ` AND lower(title) LIKE ?`
		args = append(args, "%"+strings.ToLower(query)+"%")
	}
	if cutoff := timeFilterCutoff(req.TimeFilter, c.opts.Now()); cutoff > 0 {
		where += ` AND last_activity_at>=?`
		args = append(args, cutoff)
	}
	scanCursor := cursor
	scanLimit := max(req.Limit+1, 64)
	for len(out.Items) <= req.Limit {
		pageWhere := where
		pageArgs := append([]any(nil), args...)
		if scanCursor != nil {
			pageWhere += ` AND (pinned<? OR (pinned=? AND last_activity_at<?) OR (pinned=? AND last_activity_at=? AND topic_id>?))`
			pageArgs = append(pageArgs, scanCursor.Pinned, scanCursor.Pinned, scanCursor.Activity,
				scanCursor.Pinned, scanCursor.Activity, scanCursor.TopicID)
		}
		pageArgs = append(pageArgs, scanLimit)
		rows, err := c.db.QueryContext(ctx, `SELECT scope,workspace_root,topic_id,title,pinned,sort_order,
            turns,turns_state,created_at,last_activity_at,recovery_state,recovery_branch_count,
            recovery_unresolved_count,recovery_cleanup_eligible_count,health
            FROM catalog_topics WHERE `+pageWhere+`
            ORDER BY pinned DESC,last_activity_at DESC,topic_id ASC LIMIT ?`, pageArgs...)
		if err != nil {
			return out, err
		}

		scanned := make([]TopicRecord, 0, scanLimit)
		for rows.Next() {
			var item TopicRecord
			if err := rows.Scan(&item.Scope, &item.WorkspaceRoot, &item.TopicID, &item.Title,
				&item.Pinned, &item.SortOrder, &item.Turns, &item.TurnsState,
				&item.CreatedAt, &item.LastActivityAt, &item.RecoveryState, &item.RecoveryBranchCount,
				&item.RecoveryUnresolvedCount, &item.RecoveryCleanupEligibleCount, &item.Health); err != nil {
				_ = rows.Close()
				return out, err
			}
			scanned = append(scanned, item)
		}
		rowsErr := rows.Err()
		_ = rows.Close()
		if rowsErr != nil {
			return out, rowsErr
		}
		rawCount := len(scanned)
		overflow := false
		for _, item := range scanned {
			sessions, err := c.listTopicSessions(ctx, TopicKey{
				Scope: item.Scope, WorkspaceRoot: item.WorkspaceRoot, TopicID: item.TopicID,
			})
			if err != nil {
				return TopicPage{Items: []TopicRecord{}, Revision: out.Revision}, err
			}
			if len(sessions) == 0 {
				continue
			}

			hasOrdinary := false
			for _, session := range sessions {
				if session.OrdinaryVisible || (!session.Recovered && !session.RecoveryCopy) {
					hasOrdinary = true
					break
				}
			}
			if !hasOrdinary && !item.Pinned {
				continue
			}
			item.Sessions = sessions
			item.RepresentativePath = topicRepresentativePath(sessions)
			out.Items = append(out.Items, item)
			if len(out.Items) > req.Limit {
				overflow = true
				break
			}
		}
		if overflow || rawCount < scanLimit || rawCount == 0 {
			break
		}
		lastScanned := scanned[rawCount-1]
		pinned := 0
		if lastScanned.Pinned {
			pinned = 1
		}
		scanCursor = &pageCursor{Pinned: pinned, Activity: lastScanned.LastActivityAt, TopicID: lastScanned.TopicID}
	}
	more := len(out.Items) > req.Limit
	if more {
		out.Items = out.Items[:req.Limit]
	}
	if more && len(out.Items) > 0 {
		last := out.Items[len(out.Items)-1]
		pinned := 0
		if last.Pinned {
			pinned = 1
		}
		out.NextCursor = encodeCursor(pageCursor{Pinned: pinned, Activity: last.LastActivityAt, TopicID: last.TopicID})
	}
	return out, nil
}

func (c *Catalog) listTopicSessions(ctx context.Context, key TopicKey) ([]SessionRecord, error) {
	out := []SessionRecord{}
	var cursor *sessionPageCursor
	for len(out) < MaxLimit {
		where := `scope=? AND workspace_root=? AND topic_id=?`
		args := []any{key.Scope, key.WorkspaceRoot, key.TopicID}
		if cursor != nil {
			where += ` AND (last_activity_at<? OR (last_activity_at=? AND path>?))`
			args = append(args, cursor.Activity, cursor.Activity, cursor.Path)
		}
		args = append(args, MaxLimit)
		rows, err := c.db.QueryContext(ctx, `SELECT `+sessionSelectColumns+` FROM catalog_sessions
            WHERE `+where+` ORDER BY last_activity_at DESC,path ASC LIMIT ?`, args...)
		if err != nil {
			return nil, err
		}
		rawCount := 0
		var lastScanned SessionRecord
		for rows.Next() {
			record, err := scanSession(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			rawCount++
			lastScanned = record
			if c.pathRemoved(record.Path) {
				continue
			}
			out = append(out, record)
			if len(out) == MaxLimit {
				break
			}
		}
		rowsErr := rows.Err()
		_ = rows.Close()
		if rowsErr != nil {
			return nil, rowsErr
		}
		if len(out) == MaxLimit || rawCount < MaxLimit || rawCount == 0 {
			break
		}
		cursor = &sessionPageCursor{Activity: lastScanned.LastActivityAt, Path: lastScanned.Path}
	}
	return out, nil
}

func (c *Catalog) GetTopic(ctx context.Context, key TopicKey) (TopicRecord, bool, error) {
	key.Scope, key.WorkspaceRoot = normalizeScope(key.Scope, key.WorkspaceRoot)
	key.TopicID = strings.TrimSpace(key.TopicID)
	item := TopicRecord{Sessions: []SessionRecord{}}
	err := c.db.QueryRowContext(ctx, `SELECT scope,workspace_root,topic_id,title,pinned,sort_order,
        turns,turns_state,created_at,last_activity_at,recovery_state,recovery_branch_count,
        recovery_unresolved_count,recovery_cleanup_eligible_count,health
        FROM catalog_topics WHERE scope=? AND workspace_root=? AND topic_id=?`,
		key.Scope, key.WorkspaceRoot, key.TopicID).Scan(
		&item.Scope, &item.WorkspaceRoot, &item.TopicID, &item.Title,
		&item.Pinned, &item.SortOrder, &item.Turns, &item.TurnsState,
		&item.CreatedAt, &item.LastActivityAt, &item.RecoveryState, &item.RecoveryBranchCount,
		&item.RecoveryUnresolvedCount, &item.RecoveryCleanupEligibleCount, &item.Health)
	if errors.Is(err, sql.ErrNoRows) {
		return item, false, nil
	}
	if err != nil {
		return item, false, err
	}
	item.Sessions, err = c.listTopicSessions(ctx, key)
	if err != nil {
		return TopicRecord{Sessions: []SessionRecord{}}, false, err
	}

	if len(item.Sessions) == 0 {
		return TopicRecord{Sessions: []SessionRecord{}}, false, nil
	}
	item.RepresentativePath = topicRepresentativePath(item.Sessions)
	return item, true, nil
}

func topicRepresentativePath(sessions []SessionRecord) string {
	if path := CanonicalSessionPathForTopic(sessions, ""); path != "" {
		return path
	}
	preferred := PreferredOrdinarySessionPaths(sessions)
	best := SessionRecord{}
	found := false
	for _, session := range sessions {
		path := strings.TrimSpace(session.Path)
		_, isPreferred := preferred[path]
		if !session.OrdinaryVisible && !isPreferred && (session.Recovered || session.RecoveryCopy) {
			continue
		}
		if !found || recoveryRank(session) > recoveryRank(best) ||
			(recoveryRank(session) == recoveryRank(best) && session.LastActivityAt > best.LastActivityAt) {
			best = session
			found = true
		}
	}
	if found {
		return best.Path
	}
	if len(sessions) > 0 {
		return sessions[0].Path
	}
	return ""
}

// EncodeTopicCursor builds an exclusive ListTopics keyset cursor after the
// given topic position. Desktop post-filters recovery-only rows and needs the
// same cursor shape catalog.ListTopics emits.
func EncodeTopicCursor(pinned int, lastActivityAt int64, topicID string) string {
	return encodeCursor(pageCursor{Pinned: pinned, Activity: lastActivityAt, TopicID: topicID})
}

func encodeCursor(cursor pageCursor) string {
	b, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(encoded string) (*pageCursor, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid session catalog cursor: %w", err)
	}
	var cursor pageCursor
	if err := json.Unmarshal(b, &cursor); err != nil || cursor.TopicID == "" {
		return nil, errors.New("invalid session catalog cursor")
	}
	return &cursor, nil
}

func timeFilterCutoff(filter string, now time.Time) int64 {
	var duration time.Duration
	value := strings.TrimSpace(strings.ToLower(filter))
	switch value {
	case "day", "24h":
		duration = 24 * time.Hour
	case "week", "7d":
		duration = 7 * 24 * time.Hour
	case "month", "30d":
		duration = 30 * 24 * time.Hour
	default:
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return 0
		}
		duration = parsed
	}
	return now.Add(-duration).UnixMilli()
}
