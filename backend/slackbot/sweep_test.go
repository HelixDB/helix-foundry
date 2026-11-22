package slackbot

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/agent"
)

// The sweep prunes intake and replies past foundry.slack.retention.intake
// and .replies, not a fixed week.
func TestSweepPrunesByTheConfiguredRetention(t *testing.T) {
	w := newWorld(t, func(o *Options) { o.Cfg.Retention.Intake, o.Cfg.Retention.Replies = time.Hour, time.Hour })
	w.message(ada, dmAda, ts(1), ts(1), "hello")
	w.handle()
	turn := w.only()
	w.claimTurn(turn.ID).finish(agent.StopAnswered, "")
	w.until("the reply", w.replyDone(turn.ID))

	count := func(table string) int {
		var n int
		require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE install_id=?`, w.install.ID).Scan(&n))
		return n
	}
	w.bot.Sweep(w.ctx)
	require.Equal(t, 1, count("foundry_slack_intake"), "a young row stays")
	require.Equal(t, 1, count("foundry_slack_replies"))

	_, err := testDB.Exec(`UPDATE foundry_slack_intake SET created_at=NOW(6) - INTERVAL 2 HOUR WHERE install_id=?`, w.install.ID)
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_slack_replies SET finished_at=NOW(6) - INTERVAL 2 HOUR WHERE install_id=?`, w.install.ID)
	require.NoError(t, err)
	w.bot.Sweep(w.ctx)
	assert.Zero(t, count("foundry_slack_intake"), "intake past an hour's retention")
	assert.Zero(t, count("foundry_slack_replies"), "replies past an hour's retention")
}
