//go:build integration && rollbackrehearsal

package rollbackrehearsal_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"golang.org/x/crypto/bcrypt"
)

const (
	currentSourceSHA = "6f73d6a5833266f4eca8df986dbda73e6c2f9aaf"
	oldSourceSHA     = "78f962607ba290264d718c6d43e703e7e2864b08"
	postgresImage    = "postgres:18.1-alpine3.23"
	redisImage       = "redis:8.4-alpine"
)

var expectedMigrationChecksums = map[string]string{
	"239_phone_auth_identity.sql":                    "49002f9757f06b250550fb0ce7fd26902707085524731053031d2e16f648435d",
	"240_desktop_model_credentials.sql":              "bca1d6f2f4a5922de2802b4df77009570d86170279bb1f7c3401d728b0f97cee",
	"241_desktop_credential_identity_revocation.sql": "4995fe96909d7b92663e66998d502928f20e9f699f657e96e0fc3f4f39c54a00",
	"242_desktop_usage_correlation.sql":              "83699a3440b95cd425266104b9e1011654de11c3d8dbac6a01822f58f1bd1447",
	"243_desktop_usage_indexes_notx.sql":             "3b82942b9afbb1d1050d8b5d274cf4039de094e0c4cf7675dca0dc3bf0d97894",
	"244_payment_client_order_id.sql":                "bda0c13d24c2ef720a54c9d59a71550b08e202157e4d4649eedf9b9e6799ea4f",
}

type fixtureIDs struct {
	UserID         int64 `json:"user_id"`
	GroupID        int64 `json:"group_id"`
	SubscriptionID int64 `json:"subscription_id"`
	OrderID        int64 `json:"order_id"`
	OrdinaryKeyID  int64 `json:"ordinary_key_id"`
	ManagedKeyID   int64 `json:"managed_key_id"`
	CredentialID   int64 `json:"credential_id"`
}

type dataSnapshot struct {
	IDs                  fixtureIDs        `json:"ids"`
	Balance              string            `json:"balance"`
	OrderStatus          string            `json:"order_status"`
	SubscriptionUsage    string            `json:"subscription_usage"`
	IdentitySubjects     map[string]string `json:"identity_subjects"`
	OrdinaryKeyStatus    string            `json:"ordinary_key_status"`
	ManagedKeyStatus     string            `json:"managed_key_status"`
	ManagedRevokeReason  string            `json:"managed_revoke_reason,omitempty"`
	ManagedCredentialOff bool              `json:"managed_credential_revoked"`
}

type phaseProof struct {
	Phase                       string `json:"phase"`
	ProfileStatus               int    `json:"profile_status"`
	OrdersStatus                int    `json:"orders_status"`
	SubscriptionsStatus         int    `json:"subscriptions_status"`
	KeysStatus                  int    `json:"keys_status"`
	OrdinaryKeyModelsStatus     int    `json:"ordinary_key_models_status"`
	ManagedKeyModelsStatus      int    `json:"managed_key_models_status"`
	PhoneSendDisabledStatus     int    `json:"phone_send_disabled_status"`
	PhoneSendDisabledReason     string `json:"phone_send_disabled_reason"`
	RegistrationDisabledStatus  int    `json:"registration_disabled_status"`
	PaymentCreateDisabledStatus int    `json:"payment_create_disabled_status"`
	CredentialIssueHaltedStatus int    `json:"credential_issue_halted_status"`
	CredentialIssueHaltedReason string `json:"credential_issue_halted_reason,omitempty"`
}

type processReceipt struct {
	Label      string `json:"label"`
	SourceSHA  string `json:"source_sha"`
	BinarySHA  string `json:"binary_sha256"`
	LogPath    string `json:"log_path"`
	Started    bool   `json:"started"`
	Ready      bool   `json:"ready"`
	ExitStatus int    `json:"exit_status"`
}

type rehearsalReceipt struct {
	StartedAt          time.Time                    `json:"started_at"`
	CompletedAt        time.Time                    `json:"completed_at"`
	EnvironmentKeys    []string                     `json:"environment_keys"`
	PostgresImage      string                       `json:"postgres_image"`
	RedisImage         string                       `json:"redis_image"`
	MigrationChecksums map[string]map[string]string `json:"migration_checksums"`
	Processes          []*processReceipt            `json:"processes"`
	Phases             []phaseProof                 `json:"phases"`
	Snapshots          map[string]dataSnapshot      `json:"snapshots"`
	RetainedPayment    retainedPaymentProof         `json:"retained_payment"`
	Notes              []string                     `json:"notes"`
}

type serverProcess struct {
	cmd     *exec.Cmd
	done    chan error
	receipt *processReceipt
	once    sync.Once
}

type apiEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Reason  string          `json:"reason"`
	Data    json.RawMessage `json:"data"`
}

func TestCompatibleOldServerRollbackRehearsal(t *testing.T) {
	currentBinary := requiredEnv(t, "AINO_ROLLBACK_CURRENT_BINARY")
	oldBinary := requiredEnv(t, "AINO_ROLLBACK_OLD_BINARY")
	artifactDir := requiredEnv(t, "AINO_ROLLBACK_ARTIFACT_DIR")
	receiptPath := requiredEnv(t, "AINO_ROLLBACK_RECEIPT")
	require.Equal(t, currentSourceSHA, requiredEnv(t, "AINO_ROLLBACK_CURRENT_SOURCE_SHA"))
	require.Equal(t, oldSourceSHA, requiredEnv(t, "AINO_ROLLBACK_OLD_SOURCE_SHA"))
	require.NoError(t, os.MkdirAll(artifactDir, 0o700))

	currentBinarySHA := fileSHA256(t, currentBinary)
	oldBinarySHA := fileSHA256(t, oldBinary)
	require.Equal(t, requiredEnv(t, "AINO_ROLLBACK_CURRENT_ARTIFACT_SHA256"), currentBinarySHA)
	require.Equal(t, requiredEnv(t, "AINO_ROLLBACK_OLD_ARTIFACT_SHA256"), oldBinarySHA)

	receipt := &rehearsalReceipt{
		StartedAt:          time.Now().UTC(),
		EnvironmentKeys:    []string{"AINO_ROLLBACK_ARTIFACT_DIR", "AINO_ROLLBACK_CURRENT_ARTIFACT_SHA256", "AINO_ROLLBACK_CURRENT_BINARY", "AINO_ROLLBACK_CURRENT_SOURCE_SHA", "AINO_ROLLBACK_OLD_ARTIFACT_SHA256", "AINO_ROLLBACK_OLD_BINARY", "AINO_ROLLBACK_OLD_SOURCE_SHA", "AINO_ROLLBACK_RECEIPT", "ALL_PROXY", "CONFIG_FILE", "DATA_DIR", "HOME", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "PAYMENT_RESUME_SIGNING_KEY", "SKIP_SETUP", "TZ"},
		PostgresImage:      postgresImage,
		RedisImage:         redisImage,
		MigrationChecksums: map[string]map[string]string{},
		Snapshots:          map[string]dataSnapshot{},
		Notes: []string{
			"All exercised HTTP destinations were asserted loopback.",
			"Server child processes inherited no caller environment; HTTP(S)/ALL proxy variables point to a closed loopback port, NO_PROXY permits only owned loopback services, and pricing.remote_url is blank. A transport that deliberately ignores proxy variables is outside this process-level block.",
			"SMS was globally disabled before the old binary; the old binary does not implement the newer phone rollout allowlist.",
			"A synthetic pending EasyPay order was signed and fulfilled through the compatible-old binary while payment_enabled=false; no external provider was contacted.",
		},
	}
	defer func() {
		receipt.CompletedAt = time.Now().UTC()
		writeReceipt(t, receiptPath, receipt)
	}()

	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, postgresImage, tcpostgres.WithDatabase("rollback_rehearsal"), tcpostgres.WithUsername("fixture"), tcpostgres.WithPassword("fixture"), tcpostgres.BasicWaitStrategies())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Terminate(ctx)) })
	redis, err := tcredis.Run(ctx, redisImage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, redis.Terminate(ctx)) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	require.NoError(t, err)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.PingContext(ctx))

	redisHost, err := redis.Host(ctx)
	require.NoError(t, err)
	redisPort, err := redis.MappedPort(ctx, "6379/tcp")
	require.NoError(t, err)

	address := loopbackAddress(t)
	configPath := filepath.Join(artifactDir, "config.yaml")
	writeConfig(t, configPath, dsn, redisHost, redisPort.Int(), address)
	baseURL := "http://" + address

	latestFirstReceipt := &processReceipt{Label: "latest-create", SourceSHA: currentSourceSHA, BinarySHA: currentBinarySHA, LogPath: filepath.Join(artifactDir, "latest-create.log"), ExitStatus: -1}
	receipt.Processes = append(receipt.Processes, latestFirstReceipt)
	latestFirst := startServer(t, currentBinary, configPath, baseURL, latestFirstReceipt)
	waitForReady(t, baseURL, latestFirst)

	ids, ordinaryKey, managedKey, password := seedFixture(t, db)
	retainedPayment := seedRetainedPaymentFixture(t, db, baseURL)
	receipt.RetainedPayment = newRetainedPaymentProof(retainedPayment)
	latestToken := login(t, baseURL, fixtureEmail(ids.UserID), password)
	session := tokenSessionIdentity(t, latestToken)
	seedManagedCredential(t, db, &ids, session)
	receipt.Phases = append(receipt.Phases, verifyPhase(t, "latest-before-halt", baseURL, latestToken, ids, ordinaryKey, managedKey, true, false))
	receipt.Snapshots["latest-before-rollback"] = snapshot(t, db, ids)
	receipt.RetainedPayment.BeforeRollback = retainedPaymentSnapshotState(t, db, retainedPayment)
	receipt.MigrationChecksums["latest-before-rollback"] = migrationChecksums(t, db)
	setDesktopEnabled(t, db, false)
	receipt.Phases = append(receipt.Phases, verifyPhase(t, "latest-pre-downgrade-halt", baseURL, latestToken, ids, ordinaryKey, managedKey, false, true))
	latestFirst.stop(t)

	oldReceipt := &processReceipt{Label: "compatible-old", SourceSHA: oldSourceSHA, BinarySHA: oldBinarySHA, LogPath: filepath.Join(artifactDir, "compatible-old.log"), ExitStatus: -1}
	receipt.Processes = append(receipt.Processes, oldReceipt)
	old := startServer(t, oldBinary, configPath, baseURL, oldReceipt)
	waitForReady(t, baseURL, old)
	require.Equal(t, receipt.MigrationChecksums["latest-before-rollback"], migrationChecksums(t, db), "old startup must not mutate migration ledger checksums")
	oldToken := login(t, baseURL, fixtureEmail(ids.UserID), password)
	receipt.Phases = append(receipt.Phases, verifyPhase(t, "compatible-old-before-revocation", baseURL, oldToken, ids, ordinaryKey, managedKey, false, true))
	exerciseRetainedPaymentCallback(t, db, baseURL, retainedPayment, &receipt.RetainedPayment)
	revokeDevice(t, baseURL, oldToken)
	receipt.Phases = append(receipt.Phases, verifyPhase(t, "compatible-old-after-revocation", baseURL, oldToken, ids, ordinaryKey, managedKey, false, true))
	receipt.Snapshots["compatible-old"] = snapshot(t, db, ids)
	receipt.MigrationChecksums["compatible-old"] = migrationChecksums(t, db)
	old.stop(t)

	latestRestoreReceipt := &processReceipt{Label: "latest-restore", SourceSHA: currentSourceSHA, BinarySHA: currentBinarySHA, LogPath: filepath.Join(artifactDir, "latest-restore.log"), ExitStatus: -1}
	receipt.Processes = append(receipt.Processes, latestRestoreReceipt)
	latestRestore := startServer(t, currentBinary, configPath, baseURL, latestRestoreReceipt)
	waitForReady(t, baseURL, latestRestore)
	restoreToken := login(t, baseURL, fixtureEmail(ids.UserID), password)
	receipt.Phases = append(receipt.Phases, verifyPhase(t, "latest-restored", baseURL, restoreToken, ids, ordinaryKey, managedKey, false, true))
	verifyRetainedPaymentAfterRestore(t, db, baseURL, retainedPayment, &receipt.RetainedPayment)
	receipt.Snapshots["latest-restored"] = snapshot(t, db, ids)
	receipt.MigrationChecksums["latest-restored"] = migrationChecksums(t, db)
	latestRestore.stop(t)

	before := receipt.Snapshots["latest-before-rollback"]
	afterOld := receipt.Snapshots["compatible-old"]
	afterRestore := receipt.Snapshots["latest-restored"]
	require.Equal(t, before.IDs, afterOld.IDs)
	require.Equal(t, before.IDs, afterRestore.IDs)
	require.Equal(t, before.Balance, afterOld.Balance)
	require.Equal(t, before.Balance, afterRestore.Balance)
	require.Equal(t, before.OrderStatus, afterOld.OrderStatus)
	require.Equal(t, before.OrderStatus, afterRestore.OrderStatus)
	require.Equal(t, before.SubscriptionUsage, afterOld.SubscriptionUsage)
	require.Equal(t, before.SubscriptionUsage, afterRestore.SubscriptionUsage)
	require.Equal(t, before.IdentitySubjects, afterOld.IdentitySubjects)
	require.Equal(t, before.IdentitySubjects, afterRestore.IdentitySubjects)
	require.Equal(t, before.OrdinaryKeyStatus, afterOld.OrdinaryKeyStatus)
	require.Equal(t, before.OrdinaryKeyStatus, afterRestore.OrdinaryKeyStatus)
	require.Equal(t, "active", before.ManagedKeyStatus)
	require.False(t, before.ManagedCredentialOff)
	require.Empty(t, before.ManagedRevokeReason)
	require.Equal(t, "disabled", afterOld.ManagedKeyStatus)
	require.True(t, afterOld.ManagedCredentialOff)
	require.Equal(t, "parent_session_revoked", afterOld.ManagedRevokeReason)
	require.Equal(t, "disabled", afterRestore.ManagedKeyStatus)
	require.True(t, afterRestore.ManagedCredentialOff)
	require.Equal(t, "parent_session_revoked", afterRestore.ManagedRevokeReason)
	require.Equal(t, afterOld.ManagedCredentialOff, afterRestore.ManagedCredentialOff)
	require.Equal(t, afterOld.ManagedRevokeReason, afterRestore.ManagedRevokeReason)
	require.Equal(t, receipt.MigrationChecksums["latest-before-rollback"], receipt.MigrationChecksums["compatible-old"])
	require.Equal(t, receipt.MigrationChecksums["latest-before-rollback"], receipt.MigrationChecksums["latest-restored"])
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required for the explicit rollback rehearsal", name)
	}
	return value
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()
	h := sha256.New()
	_, err = io.Copy(h, f)
	require.NoError(t, err)
	return hex.EncodeToString(h.Sum(nil))
}

func loopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	return address
}

func writeConfig(t *testing.T, path, dsn, redisHost string, redisPort int, address string) {
	t.Helper()
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	password, _ := parsed.User.Password()
	port, err := strconv.Atoi(parsed.Port())
	require.NoError(t, err)
	_, serverPortText, err := net.SplitHostPort(address)
	require.NoError(t, err)
	serverPort, err := strconv.Atoi(serverPortText)
	require.NoError(t, err)
	pricingDir := filepath.Join(filepath.Dir(path), "pricing")
	require.NoError(t, os.MkdirAll(pricingDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(pricingDir, "model_pricing.json"), []byte(`{"rollback-model":{"litellm_provider":"test","mode":"chat","input_cost_per_token":0.000001,"output_cost_per_token":0.000002}}`+"\n"), 0o600))
	content := fmt.Sprintf(`server:
  host: "127.0.0.1"
  port: %d
  mode: "release"
  trusted_proxies: []
database:
  host: %q
  port: %d
  user: %q
  password: %q
  dbname: %q
  sslmode: "disable"
redis:
  host: %q
  port: %d
  db: 0
jwt:
  secret: "rollback-rehearsal-jwt-secret-32-bytes-minimum"
  expire_hour: 1
  access_token_expire_minutes: 60
  refresh_token_expire_days: 1
totp:
  encryption_key: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
sms:
  enabled: false
pricing:
  remote_url: ""
  hash_url: ""
  data_dir: %q
token_refresh:
  enabled: false
log:
  level: "info"
  format: "console"
  output:
    to_stdout: true
    to_file: false
security:
  url_allowlist:
    enabled: true
    upstream_hosts:
      - "127.0.0.1"
    pricing_hosts:
      - "127.0.0.1"
    allow_private_hosts: true
    allow_insecure_http: true
  proxy_fallback:
    allow_direct_on_error: false
`, serverPort, parsed.Hostname(), port, parsed.User.Username(), password, strings.TrimPrefix(parsed.Path, "/"), redisHost, redisPort, pricingDir)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func startServer(t *testing.T, binary, configPath, baseURL string, receipt *processReceipt) *serverProcess {
	t.Helper()
	assertLoopbackURL(t, baseURL)
	logFile, err := os.OpenFile(receipt.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	home := filepath.Join(filepath.Dir(receipt.LogPath), "home-"+receipt.Label)
	require.NoError(t, os.MkdirAll(home, 0o700))
	cmd := exec.Command(binary)
	cmd.Env = []string{
		"ALL_PROXY=http://127.0.0.1:1",
		"CONFIG_FILE=" + configPath,
		"DATA_DIR=" + filepath.Dir(configPath),
		"HOME=" + home,
		"HTTP_PROXY=http://127.0.0.1:1",
		"HTTPS_PROXY=http://127.0.0.1:1",
		"NO_PROXY=127.0.0.1,localhost",
		"PAYMENT_RESUME_SIGNING_KEY=rollback-rehearsal-payment-signing-key",
		"SKIP_SETUP=true",
		"TZ=UTC",
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	require.NoError(t, cmd.Start())
	receipt.Started = true
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		_ = logFile.Close()
		done <- err
	}()
	process := &serverProcess{cmd: cmd, done: done, receipt: receipt}
	t.Cleanup(process.cleanup)
	return process
}

func waitForReady(t *testing.T, baseURL string, process *serverProcess) {
	t.Helper()
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case err := <-process.done:
			process.receipt.ExitStatus = exitStatus(err)
			t.Fatalf("%s exited before readiness with status %d; log=%s", process.receipt.Label, process.receipt.ExitStatus, process.receipt.LogPath)
		case <-ticker.C:
			request, err := http.NewRequest(http.MethodGet, baseURL+"/health", nil)
			require.NoError(t, err)
			response, err := client.Do(request)
			if err == nil {
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					process.receipt.Ready = true
					return
				}
			}
		case <-deadline.C:
			t.Fatalf("%s did not become ready within 60s; log=%s", process.receipt.Label, process.receipt.LogPath)
		}
	}
}

func (p *serverProcess) stop(t *testing.T) {
	t.Helper()
	p.once.Do(func() {
		require.NoError(t, p.cmd.Process.Signal(syscall.SIGTERM))
		select {
		case err := <-p.done:
			p.receipt.ExitStatus = exitStatus(err)
			require.Equal(t, 0, p.receipt.ExitStatus, "%s must shut down cleanly; log=%s", p.receipt.Label, p.receipt.LogPath)
		case <-time.After(20 * time.Second):
			_ = p.cmd.Process.Kill()
			t.Fatalf("%s did not shut down within 20s; log=%s", p.receipt.Label, p.receipt.LogPath)
		}
	})
}

func (p *serverProcess) cleanup() {
	p.once.Do(func() {
		if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			_ = p.cmd.Process.Kill()
		}
		select {
		case err := <-p.done:
			p.receipt.ExitStatus = exitStatus(err)
		case <-time.After(5 * time.Second):
			_ = p.cmd.Process.Kill()
			select {
			case err := <-p.done:
				p.receipt.ExitStatus = exitStatus(err)
			case <-time.After(2 * time.Second):
			}
		}
	})
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func seedFixture(t *testing.T, db *sql.DB) (fixtureIDs, string, string, string) {
	t.Helper()
	ctx := context.Background()
	for key, value := range map[string]string{
		"registration_enabled":                   "false",
		"payment_enabled":                        "false",
		"desktop.enabled":                        "true",
		"desktop.credential_ttl_seconds":         "3600",
		"openai_codex_version_auto_sync_enabled": "false",
	} {
		_, err := db.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES($1,$2,now()) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=EXCLUDED.updated_at`, key, value)
		require.NoError(t, err)
	}
	password := "rollback-fixture-password"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	ids := fixtureIDs{}
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO users(email,password_hash,balance,signup_source) VALUES($1,$2,37.5,'email') RETURNING id`, fixtureEmail(0), string(hash)).Scan(&ids.UserID))
	_, err = db.ExecContext(ctx, `UPDATE users SET email=$1 WHERE id=$2`, fixtureEmail(ids.UserID), ids.UserID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO auth_identities(user_id,provider_type,provider_key,provider_subject,verified_at) VALUES
		($1,'email','email',$2,now()),
		($1,'oidc','fixture-oidc','fixture-oidc-subject',now()),
		($1,'phone','phone','+8613900000099',now())`, ids.UserID, fixtureEmail(ids.UserID))
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform,subscription_type,status) VALUES('rollback-fixture','openai','subscription','active') RETURNING id`).Scan(&ids.GroupID))
	_, err = db.ExecContext(ctx, `UPDATE groups SET model_allowlist=$1::jsonb WHERE id=$2`, `{"enabled":true,"models":["rollback-model"]}`, ids.GroupID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO user_allowed_groups(user_id,group_id) VALUES($1,$2)`, ids.UserID, ids.GroupID)
	require.NoError(t, err)
	modelCatalog, err := json.Marshal([]map[string]any{{
		"id": "rollback-model", "group_id": ids.GroupID, "model": "rollback-model", "display_name": "Rollback model", "provider_label": "Fixture", "platform": "openai", "api_mode": "chat_completions", "sort_order": 0, "agent_verified": true,
		"capabilities": map[string]bool{"tools": true, "vision": false, "reasoning": false},
	}})
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES
		('desktop.models',$1,now()),('desktop.default_model_id','rollback-model',now())
		ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=EXCLUDED.updated_at`, string(modelCatalog))
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO user_subscriptions(user_id,group_id,starts_at,expires_at,daily_usage_usd,status) VALUES($1,$2,now(),now()+interval '30 days',1.25,'active') RETURNING id`, ids.UserID, ids.GroupID).Scan(&ids.SubscriptionID))
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO payment_orders(user_id,amount,pay_amount,out_trade_no,status,expires_at,payment_type) VALUES($1,20,20,'rollback-fixture-order','COMPLETED',now()+interval '1 day','alipay') RETURNING id`, ids.UserID).Scan(&ids.OrderID))
	ordinaryKey := "sk-rollback-ordinary"
	managedKey := "sk-rollback-managed"
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO api_keys(user_id,key,name,group_id,status,desktop_managed) VALUES($1,$2,'rollback ordinary',$3,'active',false) RETURNING id`, ids.UserID, ordinaryKey, ids.GroupID).Scan(&ids.OrdinaryKeyID))
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO api_keys(user_id,key,name,group_id,status,desktop_managed,expires_at) VALUES($1,$2,'rollback managed',$3,'active',true,now()+interval '1 hour') RETURNING id`, ids.UserID, managedKey, ids.GroupID).Scan(&ids.ManagedKeyID))
	return ids, ordinaryKey, managedKey, password
}

func fixtureEmail(userID int64) string {
	return fmt.Sprintf("rollback-fixture-%d@example.test", userID)
}

func setDesktopEnabled(t *testing.T, db *sql.DB, enabled bool) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `UPDATE settings SET value=$1,updated_at=now() WHERE key='desktop.enabled'`, strconv.FormatBool(enabled))
	require.NoError(t, err)
}

type sessionIdentity struct {
	FamilyID     string
	TokenVersion int64
}

func seedManagedCredential(t *testing.T, db *sql.DB, ids *fixtureIDs, session sessionIdentity) {
	t.Helper()
	require.NotEmpty(t, session.FamilyID)
	require.NoError(t, db.QueryRowContext(context.Background(), `INSERT INTO desktop_model_credentials(user_id,device_id,connection_grant_id,session_family_id,token_version,group_id,model_id,api_key_id,expires_at) VALUES($1,'11111111-1111-4111-8111-111111111111','22222222-2222-4222-8222-222222222222',$2,$3,$4,'rollback-model',$5,now()+interval '1 hour') RETURNING id`, ids.UserID, session.FamilyID, session.TokenVersion, ids.GroupID, ids.ManagedKeyID).Scan(&ids.CredentialID))
}

func login(t *testing.T, baseURL, email, password string) string {
	t.Helper()
	status, body := request(t, http.MethodPost, baseURL+"/api/v1/auth/login", fmt.Sprintf(`{"email":%q,"password":%q}`, email, password), "")
	require.Equal(t, http.StatusOK, status, "login response: %s", body)
	envelope := decodeEnvelope(t, body)
	var data struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.Unmarshal(envelope.Data, &data))
	require.NotEmpty(t, data.AccessToken)
	return data.AccessToken
}

func tokenSessionIdentity(t *testing.T, token string) sessionIdentity {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims struct {
		SessionID    string `json:"sid"`
		TokenVersion int64  `json:"token_version"`
	}
	require.NoError(t, json.Unmarshal(payload, &claims))
	require.NotEmpty(t, claims.SessionID)
	require.NotZero(t, claims.TokenVersion)
	return sessionIdentity{FamilyID: claims.SessionID, TokenVersion: claims.TokenVersion}
}

func verifyPhase(t *testing.T, phase, baseURL, token string, ids fixtureIDs, ordinaryKey, managedKey string, managedActive, issuanceHalted bool) phaseProof {
	t.Helper()
	proof := phaseProof{Phase: phase}
	proof.ProfileStatus = requireEnvelopeContains(t, http.MethodGet, baseURL+"/api/v1/user/profile", "", token, func(data json.RawMessage) {
		var profile map[string]any
		require.NoError(t, json.Unmarshal(data, &profile))
		require.Equal(t, float64(ids.UserID), profile["id"])
		require.Equal(t, fixtureEmail(ids.UserID), profile["email"])
		require.Equal(t, 37.5, profile["balance"])
		require.Equal(t, true, profile["phone_bound"])
		require.Equal(t, true, profile["oidc_bound"])
	})
	proof.OrdersStatus = requireEnvelopeContains(t, http.MethodGet, baseURL+"/api/v1/payment/orders/my", "", token, func(data json.RawMessage) {
		var page struct {
			Items []struct {
				ID     int64  `json:"id"`
				Status string `json:"status"`
			} `json:"items"`
		}
		require.NoError(t, json.Unmarshal(data, &page))
		require.Contains(t, page.Items, struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		}{ID: ids.OrderID, Status: "COMPLETED"})
	})
	proof.SubscriptionsStatus = requireEnvelopeContains(t, http.MethodGet, baseURL+"/api/v1/subscriptions", "", token, func(data json.RawMessage) {
		var items []map[string]any
		require.NoError(t, json.Unmarshal(data, &items))
		require.True(t, containsNumericID(items, ids.SubscriptionID))
	})
	proof.KeysStatus = requireEnvelopeContains(t, http.MethodGet, baseURL+"/api/v1/keys", "", token, func(data json.RawMessage) {
		var page struct {
			Items []map[string]any `json:"items"`
		}
		require.NoError(t, json.Unmarshal(data, &page))
		require.True(t, containsNumericID(page.Items, ids.OrdinaryKeyID))
		require.True(t, containsNumericID(page.Items, ids.ManagedKeyID))
	})
	proof.OrdinaryKeyModelsStatus, _ = request(t, http.MethodGet, baseURL+"/v1/models", "", ordinaryKey)
	require.Equal(t, http.StatusOK, proof.OrdinaryKeyModelsStatus)
	proof.ManagedKeyModelsStatus, _ = request(t, http.MethodGet, baseURL+"/v1/models", "", managedKey)
	if managedActive {
		require.Equal(t, http.StatusOK, proof.ManagedKeyModelsStatus)
	} else {
		require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, proof.ManagedKeyModelsStatus)
	}
	var phoneBody []byte
	proof.PhoneSendDisabledStatus, phoneBody = request(t, http.MethodPost, baseURL+"/api/v1/auth/phone/send-code", `{"phone":"13900000098"}`, "")
	require.Equal(t, http.StatusServiceUnavailable, proof.PhoneSendDisabledStatus, "phone halt response: %s", phoneBody)
	proof.PhoneSendDisabledReason = decodeEnvelope(t, phoneBody).Reason
	require.Equal(t, "SMS_DISABLED", proof.PhoneSendDisabledReason)
	proof.RegistrationDisabledStatus, _ = request(t, http.MethodPost, baseURL+"/api/v1/auth/register", `{"email":"new-user@example.test","password":"not-a-real-password"}`, "")
	require.Equal(t, http.StatusForbidden, proof.RegistrationDisabledStatus)
	proof.PaymentCreateDisabledStatus, _ = request(t, http.MethodPost, baseURL+"/api/v1/payment/orders", `{"amount":10,"payment_type":"alipay"}`, token)
	require.Equal(t, http.StatusForbidden, proof.PaymentCreateDisabledStatus)
	if issuanceHalted {
		var credentialBody []byte
		proof.CredentialIssueHaltedStatus, credentialBody = request(t, http.MethodPost, baseURL+"/api/v1/desktop/credentials", `{"device_id":"33333333-3333-4333-8333-333333333333","connection_grant_id":"44444444-4444-4444-8444-444444444444","model_id":"rollback-model"}`, token)
		require.Equal(t, http.StatusForbidden, proof.CredentialIssueHaltedStatus, "credential halt response: %s", credentialBody)
		proof.CredentialIssueHaltedReason = decodeEnvelope(t, credentialBody).Reason
		require.Equal(t, "DESKTOP_MODEL_NOT_ALLOWED", proof.CredentialIssueHaltedReason)
	}
	return proof
}

func requireEnvelopeContains(t *testing.T, method, endpoint, body, token string, check func(json.RawMessage)) int {
	t.Helper()
	status, raw := request(t, method, endpoint, body, token)
	require.Equal(t, http.StatusOK, status, "%s %s response: %s", method, endpoint, raw)
	envelope := decodeEnvelope(t, raw)
	require.Zero(t, envelope.Code)
	check(envelope.Data)
	return status
}

func decodeEnvelope(t *testing.T, raw []byte) apiEnvelope {
	t.Helper()
	var envelope apiEnvelope
	require.NoError(t, json.Unmarshal(raw, &envelope), "response: %s", raw)
	return envelope
}

func containsNumericID(items []map[string]any, id int64) bool {
	for _, item := range items {
		if item["id"] == float64(id) {
			return true
		}
	}
	return false
}

func request(t *testing.T, method, endpoint, body, token string) (int, []byte) {
	t.Helper()
	assertLoopbackURL(t, endpoint)
	request, err := http.NewRequest(method, endpoint, bytes.NewBufferString(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "aino-rollback-rehearsal")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, data
}

func assertLoopbackURL(t *testing.T, raw string) {
	t.Helper()
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	host := net.ParseIP(parsed.Hostname())
	require.NotNil(t, host, "HTTP destination must use an IP literal: %s", raw)
	require.True(t, host.IsLoopback(), "HTTP destination must be loopback: %s", raw)
}

func revokeDevice(t *testing.T, baseURL, token string) {
	t.Helper()
	status, raw := request(t, http.MethodDelete, baseURL+"/api/v1/desktop/devices/11111111-1111-4111-8111-111111111111", "", token)
	require.Equal(t, http.StatusOK, status, "device revocation response: %s", raw)
}

func snapshot(t *testing.T, db *sql.DB, ids fixtureIDs) dataSnapshot {
	t.Helper()
	ctx := context.Background()
	result := dataSnapshot{IDs: ids, IdentitySubjects: map[string]string{}}
	require.NoError(t, db.QueryRowContext(ctx, `SELECT balance::text FROM users WHERE id=$1`, ids.UserID).Scan(&result.Balance))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT status FROM payment_orders WHERE id=$1 AND user_id=$2`, ids.OrderID, ids.UserID).Scan(&result.OrderStatus))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT daily_usage_usd::text FROM user_subscriptions WHERE id=$1 AND user_id=$2`, ids.SubscriptionID, ids.UserID).Scan(&result.SubscriptionUsage))
	rows, err := db.QueryContext(ctx, `SELECT provider_type,provider_subject FROM auth_identities WHERE user_id=$1 AND provider_type IN ('email','oidc','phone') ORDER BY provider_type`, ids.UserID)
	require.NoError(t, err)
	for rows.Next() {
		var provider, subject string
		require.NoError(t, rows.Scan(&provider, &subject))
		result.IdentitySubjects[provider] = subject
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, result.IdentitySubjects, 3)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT status FROM api_keys WHERE id=$1`, ids.OrdinaryKeyID).Scan(&result.OrdinaryKeyStatus))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT status FROM api_keys WHERE id=$1`, ids.ManagedKeyID).Scan(&result.ManagedKeyStatus))
	var revokedAt sql.NullTime
	var reason sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, `SELECT revoked_at,revoke_reason FROM desktop_model_credentials WHERE id=$1`, ids.CredentialID).Scan(&revokedAt, &reason))
	result.ManagedCredentialOff = revokedAt.Valid
	result.ManagedRevokeReason = reason.String
	return result
}

func migrationChecksums(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT filename,checksum FROM schema_migrations WHERE filename >= '239_' AND filename <= '244_zzzz' ORDER BY filename`)
	require.NoError(t, err)
	result := map[string]string{}
	for rows.Next() {
		var name, checksum string
		require.NoError(t, rows.Scan(&name, &checksum))
		result[name] = checksum
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, expectedMigrationChecksums, result)
	return result
}

func writeReceipt(t *testing.T, path string, receipt *rehearsalReceipt) {
	t.Helper()
	sort.Strings(receipt.EnvironmentKeys)
	data, err := json.MarshalIndent(receipt, "", "  ")
	require.NoError(t, err)
	data = append(data, '\n')
	require.NoError(t, os.WriteFile(path, data, 0o600))
}
