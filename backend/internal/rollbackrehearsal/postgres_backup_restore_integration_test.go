//go:build integration && rollbackrehearsal

package rollbackrehearsal_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

type backupRestoreCommandReceipt struct {
	Args   []string `json:"args"`
	Exit   int      `json:"exit"`
	Label  string   `json:"label"`
	Output string   `json:"output"`
}

type backupRestoreReceipt struct {
	BinarySHA          string                        `json:"binary_sha256"`
	Commands           []backupRestoreCommandReceipt `json:"commands"`
	CompletedAt        time.Time                     `json:"completed_at"`
	DumpPath           string                        `json:"dump_path"`
	DumpSHA            string                        `json:"dump_sha256"`
	MigrationChecksums map[string]map[string]string  `json:"migration_checksums"`
	Notes              []string                      `json:"notes"`
	Processes          []*processReceipt             `json:"processes"`
	RetainedPayment    retainedPaymentProof          `json:"retained_payment"`
	RestoredDuplicate  restoredDuplicateProof        `json:"restored_duplicate_callback"`
	Snapshots          map[string]dataSnapshot       `json:"snapshots"`
	SourceDatabase     string                        `json:"source_database"`
	SourceSHA          string                        `json:"source_sha"`
	StartedAt          time.Time                     `json:"started_at"`
	TargetDatabase     string                        `json:"target_database"`
}

type restoredDuplicateProof struct {
	After  retainedPaymentSnapshot `json:"after"`
	Body   string                  `json:"body"`
	Status int                     `json:"status"`
}

func TestPostgresBackupRestoreRehearsal(t *testing.T) {
	currentBinary := requiredEnv(t, "AINO_RESTORE_CURRENT_BINARY")
	artifactDir := requiredEnv(t, "AINO_RESTORE_ARTIFACT_DIR")
	receiptPath := requiredEnv(t, "AINO_RESTORE_RECEIPT")
	require.Equal(t, currentSourceSHA, requiredEnv(t, "AINO_RESTORE_CURRENT_SOURCE_SHA"))
	currentBinarySHA := fileSHA256(t, currentBinary)
	require.Equal(t, requiredEnv(t, "AINO_RESTORE_CURRENT_ARTIFACT_SHA256"), currentBinarySHA)
	require.NoError(t, os.MkdirAll(artifactDir, 0o700))

	receipt := &backupRestoreReceipt{
		BinarySHA:          currentBinarySHA,
		DumpPath:           filepath.Join(artifactDir, "synthetic-source.dump"),
		MigrationChecksums: map[string]map[string]string{},
		Notes: []string{
			"The PostgreSQL dump was created by pg_dump inside the owned source PostgreSQL container and restored by pg_restore inside a distinct owned target PostgreSQL container.",
			"The dump contains only this test's synthetic fixture data and is retained outside Git in the owned artifact directory.",
			"The restored server used a new Redis container, so no old session or lease cache was retained.",
			"This proves one bounded synthetic backup/restore path; it does not prove lossless stale-production restores, in-flight payment reconciliation, or a deployment cutoff procedure.",
		},
		Snapshots:      map[string]dataSnapshot{},
		SourceDatabase: "rollback_restore_source",
		SourceSHA:      currentSourceSHA,
		StartedAt:      time.Now().UTC(),
		TargetDatabase: "rollback_restore_target",
	}
	defer func() {
		receipt.CompletedAt = time.Now().UTC()
		writeBackupRestoreReceipt(t, receiptPath, receipt)
	}()

	ctx := context.Background()
	sourcePG := startRestorePostgres(t, ctx, receipt.SourceDatabase)
	sourceDSN := postgresDSN(t, ctx, sourcePG)
	sourceDB := openRestoreDatabase(t, sourceDSN)
	sourceRedis := startRestoreRedis(t, ctx)
	sourceAddress := loopbackAddress(t)
	sourceConfig := filepath.Join(artifactDir, "source-config.yaml")
	writeRestoreConfig(t, sourceConfig, sourceDSN, sourceRedis, sourceAddress)
	sourceReceipt := &processReceipt{Label: "source-current", SourceSHA: currentSourceSHA, BinarySHA: receipt.BinarySHA, LogPath: filepath.Join(artifactDir, "source-current.log"), ExitStatus: -1}
	receipt.Processes = append(receipt.Processes, sourceReceipt)
	sourceServer := startServer(t, currentBinary, sourceConfig, "http://"+sourceAddress, sourceReceipt)
	waitForReady(t, "http://"+sourceAddress, sourceServer)

	ids, ordinaryKey, managedKey, password := seedFixture(t, sourceDB)
	token := login(t, "http://"+sourceAddress, fixtureEmail(ids.UserID), password)
	seedManagedCredential(t, sourceDB, &ids, tokenSessionIdentity(t, token))
	require.Equal(t, 200, verifyPhase(t, "source-before-revocation", "http://"+sourceAddress, token, ids, ordinaryKey, managedKey, true, false).ProfileStatus)
	revokeDevice(t, "http://"+sourceAddress, token)
	revokedToken := login(t, "http://"+sourceAddress, fixtureEmail(ids.UserID), password)
	require.Equal(t, 200, verifyPhase(t, "source-before-dump", "http://"+sourceAddress, revokedToken, ids, ordinaryKey, managedKey, false, false).ProfileStatus)

	retainedPayment := seedRetainedPaymentFixture(t, sourceDB, "http://"+sourceAddress)
	receipt.RetainedPayment = newRetainedPaymentProof(retainedPayment)
	receipt.RetainedPayment.BeforeRollback = retainedPaymentSnapshotState(t, sourceDB, retainedPayment)
	exerciseRetainedPaymentCallback(t, sourceDB, "http://"+sourceAddress, retainedPayment, &receipt.RetainedPayment)
	receipt.Snapshots["source-before-dump"] = snapshot(t, sourceDB, ids)
	receipt.MigrationChecksums["source-before-dump"] = migrationChecksums(t, sourceDB)
	sourceServer.stop(t)
	require.NoError(t, sourceDB.Close())

	receipt.Commands = append(receipt.Commands, runRestoreCommand(t, ctx, sourcePG, "pg_dump", []string{
		"pg_dump", "--format=custom", "--file=/tmp/synthetic-source.dump", "--username=fixture", "--dbname=" + receipt.SourceDatabase,
	}))
	copyDumpFromContainer(t, ctx, sourcePG, "/tmp/synthetic-source.dump", receipt.DumpPath)
	receipt.DumpSHA = fileSHA256(t, receipt.DumpPath)

	targetPG := startRestorePostgres(t, ctx, receipt.TargetDatabase)
	require.NoError(t, targetPG.CopyFileToContainer(ctx, receipt.DumpPath, "/tmp/synthetic-source.dump", 0o600))
	receipt.Commands = append(receipt.Commands, runRestoreCommand(t, ctx, targetPG, "pg_restore", []string{
		"pg_restore", "--exit-on-error", "--username=fixture", "--dbname=" + receipt.TargetDatabase, "/tmp/synthetic-source.dump",
	}))
	targetDSN := postgresDSN(t, ctx, targetPG)
	targetDB := openRestoreDatabase(t, targetDSN)
	targetRedis := startRestoreRedis(t, ctx)
	targetAddress := loopbackAddress(t)
	targetConfig := filepath.Join(artifactDir, "target-config.yaml")
	writeRestoreConfig(t, targetConfig, targetDSN, targetRedis, targetAddress)
	targetReceipt := &processReceipt{Label: "target-restored-current", SourceSHA: currentSourceSHA, BinarySHA: receipt.BinarySHA, LogPath: filepath.Join(artifactDir, "target-restored-current.log"), ExitStatus: -1}
	receipt.Processes = append(receipt.Processes, targetReceipt)
	targetServer := startServer(t, currentBinary, targetConfig, "http://"+targetAddress, targetReceipt)
	waitForReady(t, "http://"+targetAddress, targetServer)
	targetToken := login(t, "http://"+targetAddress, fixtureEmail(ids.UserID), password)
	require.Equal(t, 200, verifyPhase(t, "target-after-restore", "http://"+targetAddress, targetToken, ids, ordinaryKey, managedKey, false, false).ProfileStatus)
	verifyRetainedPaymentAfterRestore(t, targetDB, "http://"+targetAddress, retainedPayment, &receipt.RetainedPayment)
	receipt.RestoredDuplicate = replayRestoredPaymentCallback(t, targetDB, "http://"+targetAddress, retainedPayment, receipt.RetainedPayment.AfterCurrentRestore)
	receipt.Snapshots["target-after-restore"] = snapshot(t, targetDB, ids)
	receipt.MigrationChecksums["target-after-restore"] = migrationChecksums(t, targetDB)
	targetServer.stop(t)
	require.NoError(t, targetDB.Close())

	source := receipt.Snapshots["source-before-dump"]
	restored := receipt.Snapshots["target-after-restore"]
	require.Equal(t, source, restored, "restored durable identity, billing, subscription, and credential-revocation state must exactly match source")
	require.Equal(t, receipt.MigrationChecksums["source-before-dump"], receipt.MigrationChecksums["target-after-restore"])
	require.Equal(t, receipt.RetainedPayment.AfterDuplicate, receipt.RetainedPayment.AfterCurrentRestore)
}

func startRestorePostgres(t *testing.T, ctx context.Context, database string) *tcpostgres.PostgresContainer {
	t.Helper()
	container, err := tcpostgres.Run(ctx, postgresImage, tcpostgres.WithDatabase(database), tcpostgres.WithUsername("fixture"), tcpostgres.WithPassword("fixture"), tcpostgres.BasicWaitStrategies())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, container.Terminate(ctx)) })
	return container
}

func startRestoreRedis(t *testing.T, ctx context.Context) *tcredis.RedisContainer {
	t.Helper()
	container, err := tcredis.Run(ctx, redisImage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, container.Terminate(ctx)) })
	return container
}

func postgresDSN(t *testing.T, ctx context.Context, container *tcpostgres.PostgresContainer) string {
	t.Helper()
	dsn, err := container.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	require.NoError(t, err)
	return dsn
}

func openRestoreDatabase(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	require.NoError(t, db.PingContext(context.Background()))
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func writeRestoreConfig(t *testing.T, path, dsn string, redis *tcredis.RedisContainer, address string) {
	t.Helper()
	host, err := redis.Host(context.Background())
	require.NoError(t, err)
	port, err := redis.MappedPort(context.Background(), "6379/tcp")
	require.NoError(t, err)
	writeConfig(t, path, dsn, host, port.Int(), address)
}

func runRestoreCommand(t *testing.T, ctx context.Context, container testcontainers.Container, label string, args []string) backupRestoreCommandReceipt {
	t.Helper()
	exit, output, err := container.Exec(ctx, args, tcexec.Multiplexed())
	require.NoError(t, err)
	raw, err := io.ReadAll(output)
	require.NoError(t, err)
	receipt := backupRestoreCommandReceipt{Args: args, Exit: exit, Label: label, Output: string(raw)}
	require.Equalf(t, 0, receipt.Exit, "%s failed: %s", label, receipt.Output)
	return receipt
}

func copyDumpFromContainer(t *testing.T, ctx context.Context, container testcontainers.Container, containerPath, hostPath string) {
	t.Helper()
	reader, err := container.CopyFileFromContainer(ctx, containerPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	file, err := os.OpenFile(hostPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()
	_, err = io.Copy(file, reader)
	require.NoError(t, err)
	info, err := file.Stat()
	require.NoError(t, err)
	require.Positive(t, info.Size())
}

func writeBackupRestoreReceipt(t *testing.T, path string, receipt *backupRestoreReceipt) {
	t.Helper()
	data, err := json.MarshalIndent(receipt, "", "  ")
	require.NoError(t, err)
	data = append(data, '\n')
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func replayRestoredPaymentCallback(t *testing.T, db *sql.DB, baseURL string, fixture retainedPaymentFixture, before retainedPaymentSnapshot) restoredDuplicateProof {
	t.Helper()
	form := url.Values{
		"pid":          {retainedPaymentMerchantID},
		"out_trade_no": {fixture.OutTradeNo},
		"trade_no":     {fixture.TradeNo},
		"money":        {fixture.PayAmount},
		"trade_status": {"TRADE_SUCCESS"},
		"sign_type":    {"MD5"},
	}
	form.Set("sign", retainedEasyPaySignature(form))
	status, body := postRetainedEasyPayCallback(t, baseURL, form)
	require.Equal(t, 200, status)
	require.Equal(t, "success", body)
	after := retainedPaymentSnapshotState(t, db, fixture)
	require.Equal(t, before, after, "duplicate signed callback after restore must not re-credit or create a second audit entry")
	return restoredDuplicateProof{After: after, Body: body, Status: status}
}
