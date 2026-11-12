package reminder_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/reminder"
)

// goldens is testdata/schedule.json, which the site's mock also follows
// (copied to tests/unit/__fixtures__/reminder-schedule.json): copy it again
// when it changes.
type goldens struct {
	First []struct {
		Name string `json:"name"`
		Now  string `json:"now"`
		Zone string `json:"zone"`
		Want string `json:"want"`
	} `json:"first"`
	After []struct {
		Name string `json:"name"`
		Last string `json:"last"`
		Zone string `json:"zone"`
		Days int    `json:"days"`
		Want string `json:"want"`
	} `json:"after"`
}

func readGoldens(t *testing.T) goldens {
	t.Helper()
	raw, err := os.ReadFile("testdata/schedule.json")
	require.NoError(t, err)
	var g goldens
	require.NoError(t, json.Unmarshal(raw, &g))
	require.NotEmpty(t, g.First)
	require.NotEmpty(t, g.After)
	return g
}

func at(t *testing.T, text string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, text)
	require.NoError(t, err)
	return v
}

// Each reminder goes at 09:00 on a weekday in the person's zone, across
// weekends, changes of daylight saving and zones far from UTC.
func TestSchedule(t *testing.T) {
	g := readGoldens(t)
	for _, c := range g.First {
		t.Run("first: "+c.Name, func(t *testing.T) {
			loc, name := reminder.Zone(c.Zone)
			require.Equal(t, c.Zone, name, "a zone Foundry knows")
			got := reminder.First(at(t, c.Now), loc)
			assert.Equal(t, c.Want, got.Format(time.RFC3339))
			assert.Equal(t, time.UTC, got.Location())
			local := got.In(loc)
			assert.Equal(t, reminder.Hour, local.Hour())
			assert.NotContains(t, []time.Weekday{time.Saturday, time.Sunday}, local.Weekday())
			assert.False(t, got.Before(at(t, c.Now).Add(reminder.Lead)))
		})
	}
	for _, c := range g.After {
		t.Run("after: "+c.Name, func(t *testing.T) {
			loc, _ := reminder.Zone(c.Zone)
			got := reminder.After(at(t, c.Last), c.Days, loc)
			assert.Equal(t, c.Want, got.Format(time.RFC3339))
			assert.Equal(t, reminder.Hour, got.In(loc).Hour())
		})
	}
}

// A zone Foundry can't schedule by is UTC, never a refusal.
func TestZone(t *testing.T) {
	for _, name := range []string{"", "  ", "Local", "Mars/Olympus_Mons", "../../etc/passwd", strings.Repeat("A", reminder.MaxZone+1)} {
		loc, kept := reminder.Zone(name)
		assert.Equal(t, time.UTC, loc, "%q", name)
		assert.Equal(t, "UTC", kept, "%q", name)
	}
	loc, kept := reminder.Zone(" Asia/Kolkata ")
	assert.Equal(t, "Asia/Kolkata", kept)
	assert.Equal(t, "Asia/Kolkata", loc.String())
}

// addresses is testdata/addresses.json, which the site's mock also follows
// (copied to tests/unit/__fixtures__/reminder-addresses.json): copy it
// again when it changes.
type addresses struct {
	Kept []struct {
		In   string `json:"in"`
		Want string `json:"want"`
	} `json:"kept"`
	Refused []string `json:"refused"`
}

func TestNormalize(t *testing.T) {
	raw, err := os.ReadFile("testdata/addresses.json")
	require.NoError(t, err)
	var cases addresses
	require.NoError(t, json.Unmarshal(raw, &cases))
	require.NotEmpty(t, cases.Kept)
	require.NotEmpty(t, cases.Refused)
	for _, c := range cases.Kept {
		got, err := reminder.Normalize(c.In)
		require.NoError(t, err, "%q", c.In)
		assert.Equal(t, c.Want, got, "%q", c.In)
	}
	for _, in := range cases.Refused {
		_, err := reminder.Normalize(in)
		assert.ErrorIs(t, err, reminder.ErrEmail, "%q", in)
	}
}
