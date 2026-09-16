package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type desktopWalletRepository struct{ db *sql.DB }

func NewDesktopWalletRepository(db *sql.DB) service.DesktopWalletRepository {
	return &desktopWalletRepository{db: db}
}

func (r *desktopWalletRepository) ReadWalletBalances(ctx context.Context, userID int64) (*service.DesktopWalletBalances, error) {
	var balances service.DesktopWalletBalances
	err := r.db.QueryRowContext(ctx, `SELECT balance::text, frozen_balance::text, status
		FROM users WHERE id = $1 AND deleted_at IS NULL`, userID).
		Scan(&balances.Balance, &balances.FrozenBalance, &balances.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &balances, nil
}
