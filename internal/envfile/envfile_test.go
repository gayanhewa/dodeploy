package envfile

import "testing"

func TestSet(t *testing.T) {
	cases := []struct {
		name    string
		content string
		kv      map[string]string
		want    string
		changed bool
	}{
		{
			name:    "replaces in place and preserves everything else",
			content: "# hand written\nA=1\nB=2\n\n# another\nC=3\n",
			kv:      map[string]string{"B": "9"},
			want:    "# hand written\nA=1\nB=9\n\n# another\nC=3\n",
			changed: true,
		},
		{
			name:    "no change when the value already matches",
			content: "A=1\nB=2\n",
			kv:      map[string]string{"B": "2"},
			want:    "A=1\nB=2\n",
			changed: false,
		},
		{
			name:    "appends missing keys under a marker, sorted",
			content: "A=1\n",
			kv:      map[string]string{"Z": "9", "B": "2"},
			want:    "A=1\n\n" + managedMarker + "\nB=2\nZ=9\n",
			changed: true,
		},
		{
			name:    "an empty file gains a managed block without a leading blank",
			content: "",
			kv:      map[string]string{"K": "v"},
			want:    managedMarker + "\nK=v\n",
			changed: true,
		},
		{
			name:    "preserves an export prefix and indentation",
			content: "export A=1\n",
			kv:      map[string]string{"A": "2"},
			want:    "export A=2\n",
			changed: true,
		},
		{
			name:    "a commented assignment is not an assignment",
			content: "# A=1\nA=2\n",
			kv:      map[string]string{"A": "3"},
			want:    "# A=1\nA=3\n",
			changed: true,
		},
		{
			name:    "no keys means no change",
			content: "A=1\n",
			kv:      nil,
			want:    "A=1\n",
			changed: false,
		},
		{
			name:    "is idempotent",
			content: "A=1\n",
			kv:      map[string]string{"B": "2"},
			want:    "A=1\n\n" + managedMarker + "\nB=2\n",
			changed: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := Set(c.content, c.kv)
			if changed != c.changed {
				t.Fatalf("changed = %v, want %v", changed, c.changed)
			}
			if got != c.want {
				t.Fatalf("got:\n%q\nwant:\n%q", got, c.want)
			}
		})
	}
}

// Running the result through Set again must report no change, which is what
// lets a reconcile skip a restart.
func TestSetSecondRunIsNoop(t *testing.T) {
	first, changed := Set("A=1\n", map[string]string{"B": "2"})
	if !changed {
		t.Fatal("first run should change the file")
	}
	second, changed := Set(first, map[string]string{"B": "2"})
	if changed {
		t.Fatal("second run should be a no-op")
	}
	if first != second {
		t.Fatalf("content drifted:\n%q\n%q", first, second)
	}
}
