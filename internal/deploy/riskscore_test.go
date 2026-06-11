package deploy

import (
	"testing"
	"time"
)

func TestScoreFromSignalsClean(t *testing.T) {
	rs := ScoreFromSignals(riskSignals{LastThreeSucceeded: true})
	if rs.Score != 0 {
		t.Errorf("clean history with success streak should floor at 0, got %d", rs.Score)
	}
	if rs.Level != RiskLow {
		t.Errorf("level = %q, want low", rs.Level)
	}
}

func TestScoreFromSignalsStacked(t *testing.T) {
	rs := ScoreFromSignals(riskSignals{
		LastDeployFailed:   true, // +30
		OpenAlerts:         2,    // +20
		LogAnomaliesLastHr: 1,    // +15
		TasksDegraded:      true, // +15
	})
	if rs.Score != 80 {
		t.Errorf("score = %d, want 80", rs.Score)
	}
	if rs.Level != RiskCritical {
		t.Errorf("level = %q, want critical", rs.Level)
	}
	if len(rs.Factors) != 4 {
		t.Errorf("factors = %d, want 4", len(rs.Factors))
	}
}

func TestScoreFromSignalsLevels(t *testing.T) {
	tests := []struct {
		sig   riskSignals
		level string
	}{
		{riskSignals{}, RiskLow},                                          // 0
		{riskSignals{OpenAlerts: 1, IsFridayAfternoon: true}, RiskMedium}, // 30
		{riskSignals{LastDeployFailed: true, LogAnomaliesLastHr: 1}, RiskHigh},      // 45
		{riskSignals{LastDeployFailed: true, OpenAlerts: 1, TasksDegraded: true, CommitFailedBefore: true}, RiskCritical}, // 75
	}
	for i, tt := range tests {
		if got := ScoreFromSignals(tt.sig); got.Level != tt.level {
			t.Errorf("case %d: level = %q (score %d), want %q", i, got.Level, got.Score, tt.level)
		}
	}
}

func TestScoreFromSignalsSuccessBonusFloorsAtZero(t *testing.T) {
	rs := ScoreFromSignals(riskSignals{IsFridayAfternoon: true, LastThreeSucceeded: true})
	if rs.Score != 0 {
		t.Errorf("10 - 10 should floor at 0, got %d", rs.Score)
	}
}

func TestIsFridayAfternoonUTC(t *testing.T) {
	friday1500 := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
	if !isFridayAfternoonUTC(friday1500) {
		t.Error("Friday 15:00 UTC should be flagged")
	}
	friday1300 := time.Date(2026, 6, 12, 13, 0, 0, 0, time.UTC)
	if isFridayAfternoonUTC(friday1300) {
		t.Error("Friday 13:00 UTC should not be flagged")
	}
	monday1500 := time.Date(2026, 6, 8, 15, 0, 0, 0, time.UTC)
	if isFridayAfternoonUTC(monday1500) {
		t.Error("Monday should not be flagged")
	}
}
