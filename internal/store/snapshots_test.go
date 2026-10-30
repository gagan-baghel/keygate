package store_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// The daily analytics snapshot runs at startup and every hour, so it
// takes a day's snapshot only for the products still without one: a
// server restarted any number of times records each day once, and a day
// already recorded keeps its numbers. A product created after the day
// gets none for it.
func TestTakeMissingSnapshots(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	// Every product here is dated 2001, and every other test's product
	// later, so the snapshots taken below are only this test's.
	day := time.Date(2001, 2, 3, 15, 0, 0, 0, time.UTC)
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	product := func(name string, created time.Time) *model.Product {
		t.Helper()
		p := &model.Product{Name: name, Slug: "snap-" + strings.ToLower(name) + "-" + suffix, Type: "desktop"}
		if err := s.CreateProduct(ctx, p); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.ExecContext(ctx, "UPDATE products SET created_at = ? WHERE id = ?", created, p.ID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := s.DB.ExecContext(context.Background(), "DELETE FROM analytics_snapshots WHERE product_id = ?", p.ID); err != nil {
				t.Error(err)
			}
		})
		return p
	}
	before := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	missing := product("Missing", before)
	taken := product("Taken", before)
	later := product("Later", time.Date(2001, 3, 1, 0, 0, 0, 0, time.UTC))

	plan := &model.Plan{ProductID: missing.ID, Name: "Pro", Slug: "snap-pro-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	licence := func(n int, created time.Time) {
		t.Helper()
		l := &model.License{ProductID: missing.ID, PlanID: plan.ID, Email: "snap-" + suffix + "@example.com",
			LicenseKey: "KG-SNAP-" + suffix + "-" + string(rune('a'+n)), Status: model.StatusActive}
		if err := s.CreateLicense(ctx, l); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.ExecContext(ctx, "UPDATE licenses SET created_at = ? WHERE id = ?", created, l.ID); err != nil {
			t.Fatal(err)
		}
	}
	licence(0, before)
	licence(1, day) // new on the day

	// Taken earlier in the day, by this server before a restart or by
	// another replica.
	if err := s.CreateAnalyticsSnapshot(ctx, &model.AnalyticsSnapshot{Date: day.Truncate(24 * time.Hour),
		ProductID: taken.ID, TotalLicenses: 99}); err != nil {
		t.Fatal(err)
	}

	// Each snapshot of p as "licenses/new".
	snapshots := func(p *model.Product) []string {
		t.Helper()
		var rows []*model.AnalyticsSnapshot
		if err := s.DB.NewSelect().Model(&rows).Where("product_id = ?", p.ID).Scan(ctx); err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, r := range rows {
			out = append(out, fmt.Sprintf("%d/%d", r.TotalLicenses, r.NewLicenses))
		}
		return out
	}
	check := func(step string) {
		t.Helper()
		for _, c := range []struct {
			name string
			p    *model.Product
			want []string
		}{
			{"product without a snapshot", missing, []string{"2/1"}},
			{"product already snapshotted", taken, []string{"99/0"}},
			{"product created after the day", later, []string{}},
		} {
			if got := snapshots(c.p); !slices.Equal(got, c.want) {
				t.Errorf("%s: %s has snapshots %v (licenses/new), want %v", step, c.name, got, c.want)
			}
		}
	}

	if err := s.TakeMissingSnapshots(ctx, day); err != nil {
		t.Fatal(err)
	}
	check("first run")

	// A restart runs it again, with another licence since: the day keeps
	// the numbers it was recorded with.
	licence(2, before)
	if err := s.TakeMissingSnapshots(ctx, day); err != nil {
		t.Fatal(err)
	}
	check("after a restart")
}
