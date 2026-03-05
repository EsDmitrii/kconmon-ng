package checker

import (
	"context"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

type Target struct {
	AgentID  string
	NodeName string
	PodIP    string
	Zone     string
	Port     int
}

type Checker interface {
	Name() model.CheckType
	Check(ctx context.Context, target Target) model.CheckResult
}
