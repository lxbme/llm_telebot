package app

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func allEngagementRows(t *testing.T, s *EngagementStore) (rows []Engagement) {
	t.Helper()
	err := s.transact(false, func(tx *bolt.Tx) error {
		return tx.Bucket(engagementBucket).ForEach(func(_, data []byte) error {
			var record engagementRecord
			if err := json.Unmarshal(data, &record); err != nil {
				return err
			}
			rows = append(rows, record.Engagement)
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestEngagementRecipientProvenance(t *testing.T) {
	for _, tc := range []struct {
		name, request, aux, proposal string
		want                         bool
	}{
		{"specified other group", "Tell only group -702 when the stable release arrives.", "", "here", false},
		{"conditional before destination without comma", "When the release ships send the update to group -702.", "", "here", false},
		{"explicit other group with article", "Send the update to group -702 when the stable release arrives.", "here", "here", false},
		{"group is subject of trigger", "When maintainers notify group -702 about the stable release, tell me.", "", "here", true},
		{"group named in exclusion only", "Tell me when the stable release arrives. Do not notify group -702.", "here", "here", true},
		{"group is observation source", "Watch group -702 and tell me when the stable release arrives.", "", "", true},
		{"original group", "Tell group -701 when the stable release arrives.", "", "", true},
		{"default original group", "Tell me when the stable release arrives.", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newEngagementScript(t)
			enableTestEngagementMCP(t, s.b, newEngagementMCP(t, "release", true))
			s.intents.Store(tc.request, map[string]any{"recipient": tc.aux})
			args := regressionWatchArgs()
			args["recipient"] = tc.proposal
			if tc.name == "group is subject of trigger" {
				args["trigger"] = "When maintainers notify group -702 about the stable release"
			}
			got := sendRegressionAgreement(s, args, engagementGroup, engagementOwner, 826, "@testbot "+tc.request)
			raw := allEngagementRows(t, s.b.engagements)
			claims, err := s.b.engagements.ClaimDue(engagementNow, time.Minute, 8)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("bbolt records=%d due=%d receipt=%q", len(raw), len(claims), got)
			for _, row := range raw {
				t.Logf("status=%s version=%d recipients=%v", row.Status, row.Ref.Version, row.Current.Recipients)
			}
			if !tc.want {
				if len(raw) != 0 || len(claims) != 0 || !strings.Contains(got, "No change was saved") {
					t.Errorf("off-target request persisted or confirmed")
				}
				return
			}
			if len(raw) != 1 || len(claims) != 1 || !strings.Contains(got, "Agreement saved") ||
				raw[0].Status != EngagementActive || !slices.Equal(raw[0].Current.Recipients, []int64{engagementGroup}) {
				t.Errorf("in-scope request was not persisted and available for claim")
				return
			}
			item, err := s.b.engagements.ReadClaim(claims[0], engagementNow)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.b.verifyEngagementCapability(t.Context(), item, item.Current.Observe); err != nil {
				t.Errorf("capability no longer verifies on in-scope active item: %v", err)
			}
		})
	}
}

func TestEngagementRecipientRevision(t *testing.T) {
	for _, tc := range []struct {
		name, request, change, explicitQuote, intent string
		wantEdit                                     bool
	}{
		{"agree wrong target missing intent", "Send updates only to group -702 this week.", "agree", "", "", false},
		{"agree wrong target wrong intent", "Send updates only to group -702 this week.", "agree", "", "here", false},
		{"reaccept wrong target missing intent", "Please expand this tracking to notify group -702 when the stable release arrives.", "reaccept", "notify group -702", "", false},
		{"reaccept wrong target wrong intent", "Please expand this tracking to notify group -702 when the stable release arrives.", "reaccept", "notify group -702", "here", false},
		{"reaccept wrong target with article", "Please expand this tracking to send the update to group -702 when the stable release arrives.", "reaccept", "send the update to group -702", "here", false},
		{"reaccept same target", "Please expand this tracking to notify group -701 when the stable release arrives.", "reaccept", "notify group -701", "", true},
		{"agree excludes current chat", "Tell me this week. Do not notify this chat.", "agree", "", "", false},
		{"reaccept other Telegram user", "Please expand this tracking to notify Telegram user 72.", "reaccept", "notify Telegram user 72", "", false},
		{"agree labeled Telegram user", "Send the update to Telegram user ID: 72 this week.", "agree", "", "", false},
		{"reaccept labeled Telegram user", "Please expand this tracking to notify Telegram user ID: 72.", "reaccept", "notify Telegram user ID: 72", "here", false},
		{"reaccept verified self", "Please expand this tracking to notify Telegram user ID: 71.", "reaccept", "notify Telegram user ID: 71", "here", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newEngagementScript(t)
			enableTestEngagementMCP(t, s.b, newEngagementMCP(t, "release", true))
			got := sendRegressionAgreement(s, regressionWatchArgs(), engagementGroup, engagementOwner, 821, "@testbot Tell me when the stable release arrives.")
			raw := allEngagementRows(t, s.b.engagements)
			if len(raw) != 1 || !strings.Contains(got, "Agreement saved") {
				t.Fatalf("seed rows=%d receipt=%q", len(raw), got)
			}
			ref := raw[0].Ref
			args := map[string]any{"verdict": "accept", "recipient": "here", "change": tc.change}
			if tc.change == "reaccept" {
				args["authorization_quote"], args["once"] = tc.explicitQuote, false
			} else {
				args["duration"] = "this_week"
			}
			s.intents.Store(tc.request, map[string]any{"recipient": tc.intent})
			s.change(ref, args)
			got = s.send(engagementGroup, engagementOwner, 822, "@testbot "+tc.request)
			raw = allEngagementRows(t, s.b.engagements)
			if len(raw) != 1 {
				t.Fatalf("expected only one raw row, got %d", len(raw))
			}
			item := raw[0]
			t.Logf("status=%s version=%d recipients=%v once=%t expires=%v receipt=%q", item.Status, item.Ref.Version, item.Current.Recipients, item.Current.Once, item.Current.ExpiresAt, got)
			wantRecipient := engagementGroup
			if tc.name == "reaccept verified self" {
				wantRecipient = engagementOwner
			}
			if !slices.Equal(item.Current.Recipients, []int64{wantRecipient}) || item.Status != EngagementActive {
				t.Fatalf("seed routing or status changed")
			}
			if tc.wantEdit {
				if item.Ref.Version != ref.Version+1 || item.Current.Once || !strings.Contains(got, "Agreement saved") {
					t.Errorf("authorized reaccept missing")
				}
			} else if item.Ref != ref || !item.Current.Once || !item.Current.ExpiresAt.IsZero() || !strings.Contains(got, "No change was saved") {
				t.Errorf("off-target change rewrote bbolt state")
			}
		})
	}
}

// Serialize as Task 5 would: receipt chain and destination IDs, with no
// destination provenance. In particular, don't create it via today's dialogue.
func seedOldEngagementRoute(t *testing.T, store *EngagementStore, request string) EngagementRef {
	t.Helper()
	create := engagementFixture(engagementGroup)
	create.Decision.Request.Text = request
	return createEngagement(t, store, create)
}

func TestEngagementRecipientLegacyResume(t *testing.T) {
	s := newEngagementScript(t)
	enableTestEngagementMCP(t, s.b, newEngagementMCP(t, "release", true))
	ref := seedOldEngagementRoute(t, s.b.engagements, "Send the update to group -702 when the stable release arrives.")
	err := s.b.engagements.transact(true, func(tx *bolt.Tx) error {
		row, err := loadEngagement(tx, ref.ID)
		if err != nil {
			return err
		}
		capability := s.b.snapshot().engagementCapabilities(engagementOwner, engagementGroup)["public:upstream:release"]
		row.Current.Observe = capability
		row.Status = EngagementPaused
		return saveEngagement(tx, &row)
	})
	if err != nil {
		t.Fatal(err)
	}
	s.change(ref, map[string]any{"change": "resume", "verdict": "accept"})
	got := s.send(engagementGroup, engagementOwner, 860, "@testbot Resume tracking.")
	item, err := s.b.engagements.Get(ref.ID)
	if err != nil || item.Status != EngagementPaused || !strings.Contains(got, "Clarify") {
		t.Fatalf("unsafe legacy resume: status=%s reply=%q err=%v", item.Status, got, err)
	}
}
