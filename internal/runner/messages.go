package runner

const (
	msgRefused          = "refused: only 'deploy <app> <tag>', 'rollback <app> [tag]', 'status', 'history <app>' are accepted"
	msgUnknownApp       = "refused: app not in allowlist"
	msgBadTag           = "refused: tag must match ^sha-[0-9a-f]{7,40}$"
	msgBadMain          = "refused: bad main service"
	msgBadHPort         = "refused: bad hport"
	msgBadHMode         = "refused: bad hmode"
	msgBadCompose       = "refused: bad compose base64"
	msgComposeInvalid   = "refused: rendered compose does not parse"
	msgNoConf           = "error: app.conf not found for app"
	msgLockBusy         = "error: another deploy holds the lock (waited %s, giving up)\n"
	msgLockWait         = "waiting for deploy lock (another deploy in progress)..."
	msgLoginFail        = "error: registry login failed"
	msgPullFail         = "error: image pull failed"
	msgHealthOK         = "health: ok"
	msgHealthFail       = "health: no healthy signal within %s: %s\n"
	msgRolledBack       = "rolled back to previous sha"
	msgRedeployed       = "redeployed to requested sha"
	msgFirstFail        = "first deploy failed, app stopped (no rollback target)"
	msgRollbackFailed   = "error: rollback target is not healthy either"
	msgNoPrevious       = "error: no previous sha recorded for"
	msgTargetUnhealthy  = "error: %s target is not healthy\n"
	msgZeroDowntime     = "zero-downtime: prod never changed"
	msgStandbyStart     = "blue-green: starting standby %s on 127.0.0.1:%s -> %s\n"
	msgOverlapStart     = "blue-green: starting standby %s next to the live instance (no port)\n"
	msgOverlapOff       = "blue-green: deploy-hub.overlap is false — in-place recreate"
	msgStandbyFallback  = "blue-green: standby did not start, falling back to in-place recreate"
	msgNoStandbyPort    = "blue-green: no free standby port, falling back to in-place recreate"
	msgStandbyUnhealthy = "blue-green: standby unhealthy — discarding it, old stays live"
	msgStatefulRecreate = "blue-green: %s keeps state on a writable mount — recreating in place\n"
	msgCutover          = "blue-green: standby healthy — cutting over"
	msgRestorePrevious  = "blue-green: recreate failed after standby passed — restoring previous on permanent port"
	msgPulling          = "pulling %s\n"
	msgStarting         = "starting %s @ %s\n"
	msgRollingBack      = "rolling back to %s\n"
	msgWaitingGate      = "waiting for the health gate..."
	msgLive             = "%s — %s@%s live (%s)\n"
	msgNoHistory        = "no journal entries for %s\n"
	msgRegistryError    = "error: cannot read apps.list"
	statusHeader        = "app | sha | health | last deploy"
	statusNone          = "none"
	statusNever         = "never"
	probeOK             = "ok"
	probeFail           = "fail"
	reasonCompose       = "container unhealthy (own healthcheck)"
	reasonProcess       = "container did not stay running"
	reasonHTTP          = "no HTTP 2xx/3xx on :%s%s"
	deniedFormat        = "denied(%s)"
	deniedEmpty         = "empty"
	defaultActor        = "x"
	lastErrorFormat     = "[%s] %s pull error: %s\n"
	msgNoIdentity       = "refused: deploy identity missing (only the hub deploy workflow may deploy)"
	msgNoRepo           = "refused: app.conf has no repo= binding"
	msgUnverified       = "refused: deploy identity not verified (only the hub deploy workflow may deploy)"
	msgNotGated         = "refused: tag was never deployed successfully, it has not passed the gate"
	msgBadDigest        = "refused: digest must match ^sha256:[0-9a-f]{64}$"
	msgBadVerdict       = "refused: gate verdict missing, malformed or for another commit"
	msgGateRecorded     = "gate %s@%s: %s %s\n"
	wordPassed          = "passed"
	wordBlocked         = "blocked"
	labelYes            = "true"
	labelNo             = "false"

	verbInvalid      = "invalid"
	reasonOther      = "other"
	reasonBadRequest = "bad-request"
	reasonUnknownApp = "unknown-app"
	reasonBadTag     = "bad-tag"
	reasonBadCompose = "bad-compose"
	reasonNoIdentity = "no-identity"
	reasonUnverified = "identity-unverified"
	reasonNoRepo     = "no-repo-binding"
	reasonNotGated   = "not-gated"
	reasonBadDigest  = "bad-digest"
	reasonBadVerdict = "bad-verdict"
)

var refusalReasons = map[string]string{
	msgRefused:        reasonBadRequest,
	msgUnknownApp:     reasonUnknownApp,
	msgBadTag:         reasonBadTag,
	msgBadMain:        reasonBadCompose,
	msgBadHPort:       reasonBadCompose,
	msgBadHMode:       reasonBadCompose,
	msgBadCompose:     reasonBadCompose,
	msgComposeInvalid: reasonBadCompose,
	msgNoIdentity:     reasonNoIdentity,
	msgUnverified:     reasonUnverified,
	msgNoRepo:         reasonNoRepo,
	msgNotGated:       reasonNotGated,
	msgBadDigest:      reasonBadDigest,
	msgBadVerdict:     reasonBadVerdict,
}
