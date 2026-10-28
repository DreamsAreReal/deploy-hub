package runner

import (
	"context"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
)

const msgSecretsFail = "error: secrets not laid: "

type SecretLayer interface {
	Lay(ctx context.Context, app, dir string) ([]string, error)
}

func (s *Service) laySecrets(ctx context.Context, op *operation) bool {
	if s.Secrets == nil {
		return true
	}
	keys, err := s.Secrets.Lay(ctx, op.app, op.dir)
	if err != nil {
		s.Err.Println(msgSecretsFail + err.Error())
		s.Log.Error("secrets not laid", "app", op.app, "error", err.Error())
		s.record(journal.Entry{App: op.app, Sha7: op.sha7, Action: journal.ActionDeploy, Result: journal.ResultFail, Seconds: s.since(op.t0)})
		return false
	}
	if len(keys) > 0 {
		s.Log.Info("secrets laid", "app", op.app, "keys", strings.Join(keys, ","))
	}
	return true
}
