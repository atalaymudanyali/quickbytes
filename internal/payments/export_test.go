package payments

import (
	"context"

	"github.com/google/uuid"
)

// For the tests in package payments_test (authorize_test.go), which use the
// fake provider in paymentstest: that package imports this one, so tests
// inside this package can't import it back.
var NewOrderForTest = newOrder

// ReconcileForTest runs the reconcile loop once over these orders' payments
// only (tests share one database, so they never touch other tests' rows).
func (s *Store) ReconcileForTest(ctx context.Context, orderIDs ...uuid.UUID) error {
	ids, err := s.claimDue(ctx, orderIDs)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.sync(ctx, id, ViaReconcile); err != nil {
			return err
		}
	}
	return nil
}
