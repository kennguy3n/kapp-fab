// Package workflow defines the workflow engine interfaces that drive record
// state transitions, approval chains, and timers. Concrete implementations
// will persist runs to `workflow_runs` and emit events on transition.
package workflow
