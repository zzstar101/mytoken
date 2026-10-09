package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/relay"
	"github.com/zzstar101/mytoken/internal/source"
	"github.com/zzstar101/mytoken/internal/store"
)

type fakeClient struct {
	bills                               []relay.Bill
	err                                 error
	after                               []int64
	balanceCalls, ratioCalls, billCalls int
}

func (f *fakeClient) Kind() relay.Kind { return relay.KindNewAPI }
func (f *fakeClient) Balance(context.Context) (relay.Balance, error) {
	f.balanceCalls++
	return relay.Balance{At: time.Now()}, f.err
}
func (f *fakeClient) Snapshot(context.Context) (relay.Snapshot, error) {
	f.ratioCalls++
	return relay.Snapshot{}, f.err
}
func (f *fakeClient) Bills(_ context.Context, after int64) ([]relay.Bill, error) {
	f.billCalls++
	f.after = append(f.after, after)
	return f.bills, f.err
}
func (f *fakeClient) Daily(context.Context, time.Time, time.Time, *time.Location) ([]relay.Daily, error) {
	return nil, relay.ErrUnsupported
}

type fakeRules struct{ calls int }

func (f *fakeRules) AppendRelayRules(context.Context, string, []pricing.Rule) error {
	f.calls++
	return nil
}

func TestSyncCursorCadenceDisableAndKeyPrivacy(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const sentinel = "sk-SENTINEL-never-persist-reconcile-123456"
	cred := source.Credential{Origin: "https://relay.example", KeyID: source.KeyID(sentinel), Provider: "proxy", Secret: source.NewSecret(sentinel)}
	reads := 0
	creds := func(context.Context) ([]source.Credential, error) { reads++; return []source.Credential{cred}, nil }
	site := relay.Site{Origin: cred.Origin, KeyID: cred.KeyID, Kind: relay.KindNewAPI, Layers: relay.LayerBills, Providers: []string{"proxy"}}
	if err = st.PutRelaySite(ctx, site); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{bills: []relay.Bill{{ID: 12, At: time.Now(), Type: "consume", Model: "x", ChargedUSD: .1}}}
	syncer := NewSyncer(st, creds, func(context.Context, relay.Site, source.Credential) (relay.Client, error) { return client, nil }, nil)
	now := time.Now()
	syncer.now = func() time.Time { return now }
	syncer.runDue(ctx)
	if client.billCalls != 1 || client.balanceCalls != 0 || client.ratioCalls != 0 {
		t.Fatalf("unrequested layers called: %+v", client)
	}
	syncer.runDue(ctx)
	if client.billCalls != 1 {
		t.Fatal("synced before five minutes")
	}
	now = now.Add(5 * time.Minute)
	syncer.runDue(ctx)
	if len(client.after) != 2 || client.after[0] != 0 || client.after[1] != 12 {
		t.Fatalf("cursor sequence %v", client.after)
	}
	client.err = fmt.Errorf("upstream leaked %s in a response URL", sentinel)
	if err = syncer.Sync(ctx, site.Origin, site.KeyID); err == nil || strings.Contains(err.Error(), sentinel) {
		t.Fatalf("unsafe error %v", err)
	}
	now = now.Add(4 * time.Minute)
	syncer.runDue(ctx)
	if client.billCalls != 3 {
		t.Fatal("failure retry too soon")
	}
	now = now.Add(time.Minute)
	syncer.runDue(ctx)
	if client.billCalls != 4 {
		t.Fatal("failure retry missing")
	}
	for i := 0; i < 5; i++ {
		_ = syncer.Sync(ctx, site.Origin, site.KeyID)
	}
	state := syncer.retry[[2]string{site.Origin, site.KeyID}]
	if state.next.Sub(now) != 30*time.Minute {
		t.Fatalf("backoff %s", state.next.Sub(now))
	}
	service := NewService(st, creds, nil, nil)
	service.syncer = syncer
	statuses, err := service.Sites(ctx)
	if err != nil || len(statuses) != 1 {
		t.Fatal(statuses, err)
	}
	encoded, _ := json.Marshal(statuses)
	if strings.Contains(string(encoded), sentinel) || strings.Contains(fmt.Sprintf("%#v", statuses), sentinel) {
		t.Fatal("status leaked secret")
	}
	if statuses[0].LastError == "" || !statuses[0].HasKey {
		t.Fatal(statuses)
	}
	before := client.billCalls
	if _, err = service.Report(ctx, site.Origin, site.KeyID, time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err = service.Disable(ctx, site.Origin, site.KeyID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	syncer.runDue(ctx)
	if client.billCalls != before {
		t.Fatal("offline report or disabled background used network")
	}
	if reads < 2 {
		t.Fatal("credentials were not reread")
	}
	bills, err := st.RelayBills(ctx, site.Origin, site.KeyID, time.Time{}, time.Time{})
	if err != nil || len(bills) != 1 {
		t.Fatal("disable erased history", err)
	}
	if _, err = st.DB().ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal"} {
		raw, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), sentinel) {
			t.Fatalf("database leaked secret in %s", p)
		}
	}
}

func TestServiceDetectErrorAndReflectedResponseAreRedacted(t *testing.T) {
	const sentinel = "sk-DETECT-SENTINEL-987654321"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, sentinel, http.StatusUnauthorized)
	}))
	defer server.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cred := source.Credential{Origin: server.URL, KeyID: source.KeyID(sentinel), Secret: source.NewSecret(sentinel)}
	creds := func(context.Context) ([]source.Credential, error) { return []source.Credential{cred}, nil }
	svc := NewService(st, creds, relay.NewHTTP(server.Client()), nil)
	ctx := context.Background()
	sites, err := svc.Sites(ctx)
	if err != nil || requests != 0 || len(sites) != 1 {
		t.Fatal("site listing was not offline", sites, err)
	}
	status, err := svc.Enable(ctx, cred.Origin, cred.KeyID, relay.LayerBills)
	raw, _ := json.Marshal(status)
	if err == nil || strings.Contains(err.Error(), sentinel) || strings.Contains(string(raw), sentinel) {
		t.Fatal("detect error leaked", err)
	}
	site := relay.Site{Origin: cred.Origin, KeyID: cred.KeyID, Kind: relay.KindNewAPI, Layers: relay.LayerBills}
	if err = st.PutRelaySite(ctx, site); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{bills: []relay.Bill{{ID: 1, Model: sentinel, Type: "consume"}}}
	svc.syncer = NewSyncer(st, creds, func(context.Context, relay.Site, source.Credential) (relay.Client, error) { return client, nil }, nil)
	if err = svc.Sync(ctx, cred.Origin, cred.KeyID); err == nil || strings.Contains(err.Error(), sentinel) {
		t.Fatal("reflected key accepted", err)
	}
	bills, err := st.RelayBills(ctx, cred.Origin, cred.KeyID, time.Time{}, time.Time{})
	if err != nil || len(bills) != 0 {
		t.Fatal("unsafe response persisted")
	}
	failing := NewService(st, func(context.Context) ([]source.Credential, error) { return nil, errors.New(sentinel) }, nil, nil)
	if _, err = failing.Sites(ctx); err == nil || strings.Contains(err.Error(), sentinel) {
		t.Fatal("credential lookup error leaked")
	}
}
