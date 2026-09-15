package dns

import (
	"context"
	"fmt"
	"testing"
)

// fakeProvider records what reconciliation did, so the tests can assert on the
// sequence rather than on API calls.
type fakeProvider struct {
	records []Record
	created []Record
	updated []Record
	deleted []string
	nextID  int
}

func newFake(records ...Record) *fakeProvider {
	return &fakeProvider{records: records, nextID: 100}
}

func (f *fakeProvider) Records(context.Context, string) ([]Record, error) {
	return append([]Record{}, f.records...), nil
}

func (f *fakeProvider) Create(_ context.Context, _ string, r Record) error {
	f.nextID++
	r.ID = fmt.Sprint(f.nextID)
	f.created = append(f.created, r)
	f.records = append(f.records, r)
	return nil
}

func (f *fakeProvider) Update(_ context.Context, _ string, r Record) error {
	f.updated = append(f.updated, r)
	for i := range f.records {
		if f.records[i].ID == r.ID {
			f.records[i] = r
		}
	}
	return nil
}

func (f *fakeProvider) Delete(_ context.Context, _ string, id string) error {
	f.deleted = append(f.deleted, id)
	kept := f.records[:0]
	for _, r := range f.records {
		if r.ID != id {
			kept = append(kept, r)
		}
	}
	f.records = kept
	return nil
}

func TestSplit(t *testing.T) {
	cases := []struct {
		host, zone, sub string
	}{
		{"example.com", "example.com", ""},
		{"www.example.com", "example.com", "www"},
		{"a.b.example.com", "example.com", "a.b"},
		{"Example.COM.", "example.com", ""},
	}
	for _, c := range cases {
		zone, sub, err := Split(c.host)
		if err != nil {
			t.Fatalf("Split(%q): %v", c.host, err)
		}
		if zone != c.zone || sub != c.sub {
			t.Errorf("Split(%q) = (%q, %q), want (%q, %q)", c.host, zone, sub, c.zone, c.sub)
		}
	}
	if _, _, err := Split("localhost"); err == nil {
		t.Error("Split(localhost) should fail")
	}
}

// A registrar's parking setup is an ALIAS on the apex plus a wildcard CNAME.
// Leaving either in place means the droplet is never reached while everything
// looks configured, so both must go.
func TestEnsureARemovesParkingRecords(t *testing.T) {
	p := newFake(
		Record{ID: "1", Type: "ALIAS", Name: "example.com", Content: "pixie.porkbun.com"},
		Record{ID: "2", Type: "CNAME", Name: "*.example.com", Content: "pixie.porkbun.com"},
		Record{ID: "3", Type: "NS", Name: "example.com", Content: "ns1.porkbun.com"},
	)

	res, err := EnsureA(context.Background(), p, "example.com", "10.0.0.1", "600")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created {
		t.Error("expected the A record to be created")
	}
	if len(res.Removed) != 2 {
		t.Errorf("expected 2 shadowing records removed, got %v", res.Removed)
	}
	for _, id := range []string{"1", "2"} {
		if !contains(p.deleted, id) {
			t.Errorf("record %s should have been deleted", id)
		}
	}
	// Nameservers must never be touched.
	if contains(p.deleted, "3") {
		t.Error("an NS record was deleted")
	}
}

func TestEnsureAUpdatesExisting(t *testing.T) {
	p := newFake(Record{ID: "1", Type: "A", Name: "example.com", Content: "10.0.0.1", TTL: "600"})

	res, err := EnsureA(context.Background(), p, "example.com", "10.0.0.9", "600")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated || res.Created {
		t.Errorf("expected an update, got %+v", res)
	}
	if len(p.created) != 0 {
		t.Error("should not have created a duplicate A record")
	}
	if p.updated[0].Content != "10.0.0.9" {
		t.Errorf("content not updated: %q", p.updated[0].Content)
	}
}

func TestEnsureANoOpWhenCorrect(t *testing.T) {
	p := newFake(Record{ID: "1", Type: "A", Name: "example.com", Content: "10.0.0.1"})

	res, err := EnsureA(context.Background(), p, "example.com", "10.0.0.1", "600")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Unchanged {
		t.Errorf("expected no change, got %+v", res)
	}
	if len(p.created)+len(p.updated)+len(p.deleted) != 0 {
		t.Error("nothing should have been written")
	}
}

// The read API returns fully-qualified names; an A record must still be found.
func TestEnsureAMatchesQualifiedNames(t *testing.T) {
	p := newFake(Record{ID: "1", Type: "A", Name: "www.example.com", Content: "10.0.0.1"})

	res, err := EnsureA(context.Background(), p, "www.example.com", "10.0.0.2", "600")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated {
		t.Errorf("expected the existing record to be updated, got %+v", res)
	}
}

// A CNAME on the same name as the A record we are about to write must go, or the
// two conflict.
func TestEnsureARemovesConflictingCNAME(t *testing.T) {
	p := newFake(Record{ID: "1", Type: "CNAME", Name: "www.example.com", Content: "somewhere.example.net"})

	if _, err := EnsureA(context.Background(), p, "www.example.com", "10.0.0.1", "600"); err != nil {
		t.Fatal(err)
	}
	if !contains(p.deleted, "1") {
		t.Error("the conflicting CNAME should have been deleted")
	}
}

// A CNAME for a different name is someone else's record and must survive.
func TestEnsureALeavesOtherRecordsAlone(t *testing.T) {
	p := newFake(
		Record{ID: "1", Type: "CNAME", Name: "mail.example.com", Content: "mailhost.example.net"},
		Record{ID: "2", Type: "TXT", Name: "example.com", Content: "v=spf1 -all"},
	)

	if _, err := EnsureA(context.Background(), p, "example.com", "10.0.0.1", "600"); err != nil {
		t.Fatal(err)
	}
	if len(p.deleted) != 0 {
		t.Errorf("unrelated records were deleted: %v", p.deleted)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
