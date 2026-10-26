package payments

// For the tests in package payments_test (authorize_test.go), which use the
// fake provider in paymentstest: that package imports this one, so tests
// inside this package can't import it back.
var NewOrderForTest = newOrder
