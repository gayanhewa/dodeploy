// Package dns reconciles DNS records across providers.
//
// The Provider interface speaks fully-qualified names ("www.example.com",
// "example.com"). Providers that use a different spelling on the wire convert
// internally: Porkbun, for instance, returns fully-qualified names from its read
// API but expects the bare host from its write API, which is exactly the kind of
// asymmetry that silently produces duplicate records.
package dns

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Record is one DNS record, with Name always fully qualified.
type Record struct {
	ID      string
	Type    string
	Name    string
	Content string
	TTL     string
}

// Provider is a DNS host.
type Provider interface {
	// Records returns every record in the zone that contains hostname.
	Records(ctx context.Context, hostname string) ([]Record, error)
	Create(ctx context.Context, hostname string, r Record) error
	Update(ctx context.Context, hostname string, r Record) error
	Delete(ctx context.Context, hostname string, id string) error
}

// Result describes what reconciling one record changed.
type Result struct {
	Hostname string
	// Value is the address for an A record, or the content for a TXT record.
	Value     string
	Created   bool
	Updated   bool
	Unchanged bool
	Removed   []string // records deleted because they would shadow the record
}

// String renders the outcome for a log line.
func (r Result) String() string {
	switch {
	case r.Unchanged:
		return fmt.Sprintf("%s already correct", r.Hostname)
	case r.Created:
		return fmt.Sprintf("created %s -> %s", r.Hostname, r.Value)
	case r.Updated:
		return fmt.Sprintf("updated %s -> %s", r.Hostname, r.Value)
	default:
		return fmt.Sprintf("%s left alone", r.Hostname)
	}
}

// EnsureA points hostname at ip.
//
// It also removes records that would shadow or override the A record: an ALIAS or
// CNAME on the same name, and a wildcard CNAME. Registrars commonly ship parking
// records of both kinds on a new domain, and leaving them in place means the
// droplet is never reached while everything appears configured.
//
// NS records are never touched.
func EnsureA(ctx context.Context, p Provider, hostname, ip, ttl string) (Result, error) {
	res := Result{Hostname: hostname, Value: ip}
	if ttl == "" {
		ttl = "600"
	}

	records, err := p.Records(ctx, hostname)
	if err != nil {
		return res, err
	}

	// Remove anything that would take precedence over the record we are writing.
	for _, r := range records {
		if !shadows(r, hostname) {
			continue
		}
		if err := p.Delete(ctx, hostname, r.ID); err != nil {
			return res, fmt.Errorf("remove shadowing %s record on %s: %w", r.Type, r.Name, err)
		}
		res.Removed = append(res.Removed, fmt.Sprintf("%s %s", r.Type, r.Name))
	}

	// An A record on this exact name is updated rather than duplicated.
	for _, r := range records {
		if strings.EqualFold(r.Type, "A") && sameName(r.Name, hostname) {
			if r.Content == ip {
				res.Unchanged = true
				return res, nil
			}
			r.Content = ip
			r.TTL = ttl
			if err := p.Update(ctx, hostname, r); err != nil {
				return res, fmt.Errorf("update %s: %w", hostname, err)
			}
			res.Updated = true
			return res, nil
		}
	}

	if err := p.Create(ctx, hostname, Record{Type: "A", Name: hostname, Content: ip, TTL: ttl}); err != nil {
		return res, fmt.Errorf("create %s: %w", hostname, err)
	}
	res.Created = true
	return res, nil
}

// EnsureTXT adds a TXT record, leaving every other record in place.
//
// This is deliberately not a mirror of EnsureA. Several TXT records legitimately
// share one name: SPF, DKIM, and a domain verification token commonly sit on the
// same apex. Replacing the name's contents the way a conflicting CNAME is
// replaced would silently break email authentication, so this only adds the
// record when the same value is not already present.
func EnsureTXT(ctx context.Context, p Provider, hostname, value, ttl string) (Result, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Result{}, fmt.Errorf("a TXT value is required")
	}
	if ttl == "" {
		ttl = "600"
	}

	res := Result{Hostname: hostname, Value: value}

	records, err := p.Records(ctx, hostname)
	if err != nil {
		return res, err
	}

	for _, r := range records {
		if !strings.EqualFold(r.Type, "TXT") || !sameName(r.Name, hostname) {
			continue
		}
		if strings.TrimSpace(r.Content) == value {
			res.Unchanged = true
			return res, nil
		}
	}

	err = p.Create(ctx, hostname, Record{Type: "TXT", Name: hostname, Content: value, TTL: ttl})
	if err != nil {
		return res, fmt.Errorf("create TXT on %s: %w", hostname, err)
	}
	res.Created = true
	return res, nil
}

// shadows reports whether a record would prevent an A record on hostname from
// being used.
func shadows(r Record, hostname string) bool {
	typ := strings.ToUpper(strings.TrimSpace(r.Type))
	if typ != "ALIAS" && typ != "CNAME" {
		return false
	}
	return sameName(r.Name, hostname) || isWildcard(r.Name)
}

func sameName(a, b string) bool {
	return strings.EqualFold(normalise(a), normalise(b))
}

func isWildcard(name string) bool {
	n := normalise(name)
	return n == "*" || strings.HasPrefix(n, "*.")
}

func normalise(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// Split divides a hostname into its registrable zone and the label before it.
// "www.example.com" -> ("example.com", "www"); "example.com" -> ("example.com", "").
func Split(hostname string) (zone, subdomain string, err error) {
	hostname = normalise(hostname)
	parts := strings.Split(hostname, ".")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("cannot determine a registrable domain from %q", hostname)
	}
	if len(parts) == 2 {
		return hostname, "", nil
	}
	return strings.Join(parts[len(parts)-2:], "."), strings.Join(parts[:len(parts)-2], "."), nil
}

// SortRecords gives a stable order, mostly so output is diffable.
func SortRecords(records []Record) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].Name != records[j].Name {
			return records[i].Name < records[j].Name
		}
		return records[i].Type < records[j].Type
	})
}
