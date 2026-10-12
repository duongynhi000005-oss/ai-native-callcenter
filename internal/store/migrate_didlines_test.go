// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
)

// 00040 adds dids.max_outbound_channels. Numbers that already exist keep no
// limit, the database refuses a limit below one, a limit written through the
// catalog reads back, and the Down drops the column cleanly even when some
// numbers carry a limit.
func TestMigrationAddsTheOutboundLineLimitToExistingNumbers(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 39); err != nil {
		t.Fatalf("migrating to 38 failed: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO dids (id, number, language, allow_inbound, allow_outbound)
		 VALUES ($1, '95201', 'en', false, true)`, uuid.NewString()); err != nil {
		t.Fatalf("seed a number: %v", err)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 40); err != nil {
		t.Fatalf("migrating a database that already holds numbers failed: %v", err)
	}
	var limit *int
	if err := db.QueryRowContext(ctx,
		`SELECT max_outbound_channels FROM dids WHERE number = '95201'`).Scan(&limit); err != nil {
		t.Fatal(err)
	}
	if limit != nil {
		t.Errorf("an existing number got the limit %d, want none", *limit)
	}
	for _, bad := range []int{0, -1} {
		if _, err := db.ExecContext(ctx,
			`UPDATE dids SET max_outbound_channels = $1 WHERE number = '95201'`, bad); err == nil {
			t.Errorf("the database accepted a limit of %d", bad)
		}
	}

	st, err := Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	three := 3
	made, err := st.Catalog().CreateDID(ctx, catalog.DID{
		ID: uuid.Must(uuid.NewV7()), Number: "95202", Language: "en", IsEnabled: true,
		AllowOutbound: true, MaxOutboundChannels: &three,
	})
	if err != nil || made.MaxOutboundChannels == nil || *made.MaxOutboundChannels != 3 {
		t.Fatalf("created %+v, %v: the limit did not come back", made, err)
	}
	made.MaxOutboundChannels = nil
	updated, err := st.Catalog().UpdateDID(ctx, made)
	if err != nil || updated.MaxOutboundChannels != nil {
		t.Fatalf("cleared %+v, %v: writing the number without a limit must remove it", updated, err)
	}
	made.MaxOutboundChannels = &three
	if _, err := st.Catalog().UpdateDID(ctx, made); err != nil {
		t.Fatal(err)
	}

	if err := goose.DownToContext(ctx, db, "migrations", 39); err != nil {
		t.Fatalf("rolling 00040 back with limits set failed: %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 40); err != nil {
		t.Fatalf("migrating forward again failed: %v", err)
	}
}
