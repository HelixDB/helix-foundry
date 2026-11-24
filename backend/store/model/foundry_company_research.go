package model

import (
	"encoding/json"
	"time"
)

// FoundryCompanyResearchKind is which call a company research row records.
type FoundryCompanyResearchKind string

const (
	// FoundryCompanyResearchOfSite is a research of a company's website
	// (ResearchFoundryCompany): its row holds the research.
	FoundryCompanyResearchOfSite FoundryCompanyResearchKind = "research"
	// FoundryCompanyResearchGoal is a goal drafted from one
	// (DraftFoundryGoal): its row records the spend alone.
	FoundryCompanyResearchGoal FoundryCompanyResearchKind = "goal"
)

// FoundryCompanyResearch is one model call of a new ontology's company
// research (migrations/20261224000000_foundry_company_research.sql).
type FoundryCompanyResearch struct {
	ID          string
	WorkspaceID string
	UserID      string
	Kind        FoundryCompanyResearchKind
	// Domain is the registrable domain read; a goal's, its research's.
	Domain string
	// Result is a research's, as package companyresearch kept it; nil
	// until it ends, and for a goal or a call that failed.
	Result    json.RawMessage
	USDMicros int64
	CreatedAt time.Time
}

// FoundryCompanyResearchLimits bound a call before it is made: the caller's
// calls in the past hour, of both kinds, and the workspace's spend since
// Since (the month's start) with the call's worst case, which its row holds
// until it ends.
type FoundryCompanyResearchLimits struct {
	PerUserPerHour int
	Since          time.Time
	WorstUSDMicros int64
	MonthlyMicros  int64
}

// FoundryCompanyResearchSpend is what an ended call is charged.
type FoundryCompanyResearchSpend struct {
	// USDMicros is what the provider reported.
	USDMicros int64
	// CutShort is a call its deadline or its caller ended: the provider may
	// have billed output it never reported, so the row keeps the larger of
	// its reservation and USDMicros.
	CutShort bool
}
