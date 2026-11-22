package slack_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/slack"
)

// Slack's documented example: its secret, timestamp, body and signature.
const (
	exampleSecret    = "8f742231b10e8888abcd99yyyzzz85a5"
	exampleTimestamp = "1531420618"
	exampleBody      = "token=xyzz0WbapA4vBCDEFasx0q6G&team_id=T1DC2JH3J&team_domain=testteamnow&channel_id=G8PSS9T3V&channel_name=foobar&user_id=U2CERLKJA&user_name=roadrunner&command=%2Fwebhook-collect&text=&response_url=https%3A%2F%2Fhooks.slack.com%2Fcommands%2FT1DC2JH3J%2F397700885554%2F96rGlfmibIGlgcZRskXaIFfN&trigger_id=398738663015.47445629121.803a0bc887a14d10d2c447fce8b6703c"
	exampleSignature = "v0=a2114d57b48eac39b9ad189dd8316235a7b4a8d21a10bd27519666489c69b503"
)

func TestVerify(t *testing.T) {
	signedAt := time.Unix(1531420618, 0)
	require.Equal(t, exampleSignature, slack.Sign(exampleSecret, exampleTimestamp, []byte(exampleBody)))
	require.NoError(t, slack.Verify(exampleSecret, exampleTimestamp, []byte(exampleBody), exampleSignature, signedAt))
	// Within the window either way, to the second.
	require.NoError(t, slack.Verify(exampleSecret, exampleTimestamp, []byte(exampleBody), exampleSignature, signedAt.Add(slack.MaxSkew)))
	require.NoError(t, slack.Verify(exampleSecret, exampleTimestamp, []byte(exampleBody), exampleSignature, signedAt.Add(-slack.MaxSkew)))

	flipped := exampleSignature[:len(exampleSignature)-1] + "4"
	for name, c := range map[string]struct {
		secret, timestamp, body, signature string
		now                                time.Time
		want                               error
	}{
		"late":              {exampleSecret, exampleTimestamp, exampleBody, exampleSignature, signedAt.Add(slack.MaxSkew + time.Second), slack.ErrStale},
		"early":             {exampleSecret, exampleTimestamp, exampleBody, exampleSignature, signedAt.Add(-slack.MaxSkew - time.Second), slack.ErrStale},
		"other body":        {exampleSecret, exampleTimestamp, exampleBody + "&x=1", exampleSignature, signedAt, slack.ErrBadSignature},
		"other secret":      {"other", exampleTimestamp, exampleBody, exampleSignature, signedAt, slack.ErrBadSignature},
		"empty secret":      {"", exampleTimestamp, exampleBody, slack.Sign("", exampleTimestamp, []byte(exampleBody)), signedAt, slack.ErrBadSignature},
		"flipped":           {exampleSecret, exampleTimestamp, exampleBody, flipped, signedAt, slack.ErrBadSignature},
		"other timestamp":   {exampleSecret, "1531420619", exampleBody, exampleSignature, signedAt, slack.ErrBadSignature},
		"no signature":      {exampleSecret, exampleTimestamp, exampleBody, "", signedAt, slack.ErrMalformed},
		"v1":                {exampleSecret, exampleTimestamp, exampleBody, "v1=" + exampleSignature[3:], signedAt, slack.ErrMalformed},
		"short":             {exampleSecret, exampleTimestamp, exampleBody, exampleSignature[:66], signedAt, slack.ErrMalformed},
		"uppercase hex":     {exampleSecret, exampleTimestamp, exampleBody, strings.ToUpper(exampleSignature[:3]) + strings.ToUpper(exampleSignature[3:]), signedAt, slack.ErrMalformed},
		"not hex":           {exampleSecret, exampleTimestamp, exampleBody, "v0=" + strings.Repeat("g", 64), signedAt, slack.ErrMalformed},
		"no timestamp":      {exampleSecret, "", exampleBody, exampleSignature, signedAt, slack.ErrMalformed},
		"signed timestamp":  {exampleSecret, "+1531420618", exampleBody, exampleSignature, signedAt, slack.ErrMalformed},
		"decimal timestamp": {exampleSecret, "1531420618.5", exampleBody, exampleSignature, signedAt, slack.ErrMalformed},
		"huge timestamp":    {exampleSecret, "9999999999999", exampleBody, exampleSignature, signedAt, slack.ErrMalformed},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, slack.Verify(c.secret, c.timestamp, []byte(c.body), c.signature, c.now), c.want)
		})
	}
}

// Precheck refuses what Verify would refuse for its form, with no secret.
func TestPrecheck(t *testing.T) {
	now := time.Unix(1531420618, 0)
	require.NoError(t, slack.Precheck(exampleTimestamp, exampleSignature, now))
	require.ErrorIs(t, slack.Precheck(exampleTimestamp, "v0=", now), slack.ErrMalformed)
	require.ErrorIs(t, slack.Precheck(strconv.FormatInt(now.Add(-time.Hour).Unix(), 10), exampleSignature, now), slack.ErrStale)
}

func ExampleVerify() {
	body := []byte(`{"type":"url_verification","challenge":"c"}`)
	now := time.Unix(1795000000, 0)
	timestamp := strconv.FormatInt(now.Unix(), 10)
	signature := slack.Sign("signing-secret", timestamp, body)
	fmt.Println(slack.Verify("signing-secret", timestamp, body, signature, now))
	fmt.Println(slack.Verify("another-secret", timestamp, body, signature, now))
	fmt.Println(slack.Verify("signing-secret", timestamp, body, signature, now.Add(6*time.Minute)))
	// Output:
	// <nil>
	// slack: signature mismatch
	// slack: request timestamp outside the window
}

func FuzzVerify(f *testing.F) {
	f.Add(exampleSecret, exampleTimestamp, []byte(exampleBody), exampleSignature)
	f.Fuzz(func(t *testing.T, secret, timestamp string, body []byte, signature string) {
		now := time.Unix(1531420618, 0)
		err := slack.Verify(secret, timestamp, body, signature, now)
		if err == nil {
			// It accepts only Sign's own signature, made with a secret.
			require.NotEmpty(t, secret)
			require.Equal(t, slack.Sign(secret, timestamp, body), signature)
		}
	})
}
