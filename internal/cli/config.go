package cli

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/caddy"
	"github.com/DreamsAreReal/deploy-hub/internal/health"
	"github.com/DreamsAreReal/deploy-hub/internal/hub"
	"github.com/DreamsAreReal/deploy-hub/internal/oidc"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	envHubDir      = "HUB_DIR"
	envCaddyfile   = "CADDYFILE"
	envHostSlug    = "HOST_SLUG"
	envACMEEmail   = "ACME_EMAIL"
	envCaddyUser   = "CADDY_USER"
	envDeployUser  = "DEPLOY_USER"
	envMetricsURL  = "DEPLOY_HUB_METRICS_URL"
	envLogLevel    = "DEPLOY_HUB_LOG_LEVEL"
	envLogFile     = "DEPLOY_HUB_LOG_FILE"
	envHostAlias   = "VPS_HOST_ALIAS"
	envVPSAddr     = "VPS_ADDR"
	envDeployKey   = "DEPLOY_KEY"
	envHubRepo     = "HUB_REPO"
	envRepoOwner   = "REPO_OWNER"
	envHome        = "HOME"
	envSSHOriginal = "SSH_ORIGINAL_COMMAND"
	envGateTimeout = "DEPLOY_HUB_HEALTH_TIMEOUT"
	envGateTick    = "DEPLOY_HUB_HEALTH_INTERVAL"
	envAudience    = "DEPLOY_HUB_OIDC_AUDIENCE"
	envJWKSURL     = "DEPLOY_HUB_JWKS_URL"
	envJWKSCache   = "DEPLOY_HUB_JWKS_CACHE"
	envStoreFile   = "DEPLOY_HUB_SECRETS_FILE"
	envTextfileDir = "DEPLOY_HUB_TEXTFILE_DIR"
	envStoreAge    = "DEPLOY_HUB_SECRETS_KEY"
	envAge         = "DEPLOY_HUB_AGE"
	defaultAge     = "/usr/bin/age"
	defaultAgeKey  = "/run/deploy-hub/age.key"

	minGateTimeout = 60 * time.Second
	maxGateTimeout = 10 * time.Minute
	minGateTick    = time.Second
	maxGateTick    = 30 * time.Second

	defaultMetricsURL = "http://127.0.0.1:8428/api/v1/import/prometheus"
	defaultHostAlias  = "vpn"
	defaultDeployKey  = ".ssh/deploy_hub_key"
	defaultHubRepo    = "DreamsAreReal/deploy-hub"
	defaultRepoOwner  = "DreamsAreReal"
	defaultDeployUser = "deploy"
	defaultAudience   = "deploy-hub"
	defaultJWKSCache  = ".cache/deploy-hub/jwks.json"
	deployWorkflowRef = "%s/.github/workflows/deploy.yml@refs/heads/main"
	acmeEmailFormat   = "admin@%s.sslip.io"
	confHostSlug      = "host_slug"
	confACMEEmail     = "acme_email"
	schemeHTTP        = "http"
	schemeHTTPS       = "https"
)

var (
	ErrHubDir     = errors.New("invalid HUB_DIR: must be an absolute path")
	ErrMetricsURL = errors.New("invalid DEPLOY_HUB_METRICS_URL: must be an http(s) URL or empty")
	ErrTextfile   = errors.New("invalid DEPLOY_HUB_TEXTFILE_DIR: must be an absolute path or empty")
	ErrUser       = errors.New("invalid user name")
	ErrGate       = errors.New("invalid health gate timing: timeout 60s..10m, interval 1s..30s")
	ErrJWKSURL    = errors.New("invalid DEPLOY_HUB_JWKS_URL: must be an http(s) URL")
)

type Env func(key string) (string, bool)

func (e Env) get(key, fallback string) string {
	if v, _ := e(key); v != "" {
		return v
	}
	return fallback
}

type Config struct {
	Paths       hub.Paths
	Caddy       caddy.Config
	CaddyUser   string
	DeployUser  string
	MetricsURL  string
	TextfileDir string
	LogLevel    string
	LogFile     string
	HostAlias   string
	VPSAddr     string
	DeployKey   string
	HubRepo     string
	RepoOwner   string
	SSHCommand  string
	GateWait    time.Duration
	GateTick    time.Duration
	Audience    string
	JWKSURL     string
	JWKSCache   string
	Secrets     SecretsConfig
	SpoolRoot   string
}

type SecretsConfig struct {
	File string
	Key  string
	Age  string
}

func (c Config) WorkflowRef() string {
	return fmt.Sprintf(deployWorkflowRef, c.HubRepo)
}

func LoadConfig(env Env) (Config, error) {
	paths := hub.Paths{Dir: env.get(envHubDir, hub.DefaultDir)}
	server, _ := registry.ReadKV(paths.HubConf())
	slug := env.get(envHostSlug, server[confHostSlug])
	c := Config{
		Paths:       paths,
		Caddy:       caddy.Config{Caddyfile: env.get(envCaddyfile, caddy.DefaultCaddyfile), HostSlug: slug, Email: env.get(envACMEEmail, acmeEmail(server[confACMEEmail], slug))},
		CaddyUser:   env.get(envCaddyUser, caddy.DefaultUser),
		DeployUser:  env.get(envDeployUser, defaultDeployUser),
		MetricsURL:  defaultMetricsURL,
		LogLevel:    env.get(envLogLevel, ""),
		LogFile:     env.get(envLogFile, ""),
		HostAlias:   env.get(envHostAlias, defaultHostAlias),
		VPSAddr:     env.get(envVPSAddr, ""),
		DeployKey:   env.get(envDeployKey, filepath.Join(env.get(envHome, ""), defaultDeployKey)),
		HubRepo:     env.get(envHubRepo, defaultHubRepo),
		RepoOwner:   env.get(envRepoOwner, defaultRepoOwner),
		SSHCommand:  env.get(envSSHOriginal, ""),
		Audience:    env.get(envAudience, defaultAudience),
		JWKSURL:     env.get(envJWKSURL, oidc.JWKSURL),
		JWKSCache:   env.get(envJWKSCache, filepath.Join(env.get(envHome, ""), defaultJWKSCache)),
		Secrets:     SecretsConfig{File: env.get(envStoreFile, ""), Key: env.get(envStoreAge, defaultAgeKey), Age: env.get(envAge, defaultAge)},
		SpoolRoot:   env.get(envSpoolRoot, defaultSpool),
		TextfileDir: env.get(envTextfileDir, ""),
	}
	if v, ok := env(envMetricsURL); ok {
		c.MetricsURL = v
	}
	c.GateWait = duration(env, envGateTimeout, health.Timeout)
	c.GateTick = duration(env, envGateTick, health.Interval)
	return c, c.validate()
}

func acmeEmail(configured, slug string) string {
	if configured != "" || slug == "" {
		return configured
	}
	return fmt.Sprintf(acmeEmailFormat, slug)
}

func duration(env Env, key string, fallback time.Duration) time.Duration {
	d, _ := time.ParseDuration(env.get(key, fallback.String()))
	return d
}

func (c Config) validGate() bool {
	return c.GateWait >= minGateTimeout && c.GateWait <= maxGateTimeout &&
		c.GateTick >= minGateTick && c.GateTick <= maxGateTick
}

func (c Config) validate() error {
	if !filepath.IsAbs(c.Paths.Dir) {
		return ErrHubDir
	}
	if !c.validGate() {
		return ErrGate
	}
	if err := c.validateEndpoints(); err != nil {
		return err
	}
	if validate.AppName(c.CaddyUser) != nil || validate.AppName(c.DeployUser) != nil {
		return ErrUser
	}
	return c.validateOptional()
}

func (c Config) validateEndpoints() error {
	if err := validMetricsURL(c.MetricsURL); err != nil {
		return err
	}
	if c.TextfileDir != "" && !filepath.IsAbs(c.TextfileDir) {
		return ErrTextfile
	}
	if validMetricsURL(c.JWKSURL) != nil {
		return ErrJWKSURL
	}
	return nil
}

func (c Config) validateOptional() error {
	errs := []error{validate.RepoSlug(c.HubRepo)}
	if c.Caddy.HostSlug != "" || c.Caddy.Email != "" {
		errs = append(errs, c.Caddy.Validate())
	}
	if c.VPSAddr != "" {
		errs = append(errs, validate.Host(c.VPSAddr))
	}
	return errors.Join(errs...)
}

func validMetricsURL(v string) error {
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS) || u.Host == "" {
		return ErrMetricsURL
	}
	return nil
}
