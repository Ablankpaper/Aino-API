//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDesktopLeaseCannotChangeBillingGroupViaFallback(t *testing.T) {
	id, next := int64(10), int64(11)
	repo := &mockGroupRepoForGateway{groups: map[int64]*Group{
		id:   {ID: id, Status: StatusActive, ClaudeCodeOnly: true, FallbackGroupID: &next},
		next: {ID: next, Status: StatusActive},
	}}
	svc := &GatewayService{groupRepo: repo}
	_, resolved, err := svc.resolveGatewayGroup(context.Background(), &id)
	require.NoError(t, err)
	require.Equal(t, next, *resolved)
	ctx := WithDesktopCredentialGroup(context.Background(), id)
	_, _, err = svc.resolveGatewayGroup(ctx, &id)
	require.ErrorIs(t, err, ErrClaudeCodeOnly)
	_, _, err = svc.resolveGatewayGroup(ctx, &next)
	require.Error(t, err)
}
