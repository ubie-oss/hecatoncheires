package env

// NewFakeSlackForTest lets the Slack replay tests drive the facade without a worker.
var NewFakeSlackForTest = newFakeSlack

// TurnFinishedForTest exposes the completion predicate without polling.
var TurnFinishedForTest = turnFinished
