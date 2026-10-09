package reconcile

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zzstar101/mytoken/internal/relay"
)

// A persisted marker survives process restarts. Initial enable and changed
// pricing/aliases run the cold matching path during Sync, never during Report.
func (s *Syncer) reconcile(ctx context.Context, site relay.Site) error {
	account, _ := json.Marshal([2]string{site.Origin, site.KeyID})
	key := "relay-reconcile:" + string(account)
	rows, err := s.st.DB().QueryContext(ctx, `SELECT key,value FROM settings WHERE key IN ('pricing_fingerprint','model-aliases')`)
	if err != nil {
		return err
	}
	values := map[string]string{}
	for rows.Next() {
		var k, v string
		if err = rows.Scan(&k, &v); err != nil {
			rows.Close()
			return err
		}
		values[k] = v
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(struct {
		Settings  map[string]string
		Providers []string
	}{values, site.Providers})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(raw))
	var previous string
	err = s.st.DB().QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", key).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && previous == fingerprint {
		return ReconcilePending(ctx, s.st, site)
	}
	if _, err = Reconcile(ctx, s.st, site, time.Time{}, time.Time{}); err != nil {
		return err
	}
	return s.st.Exec(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, fingerprint)
}
