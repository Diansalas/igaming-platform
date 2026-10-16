//go:build stagingacceptance

package main

func (w *world) ledgerChecks()         {}
func (w *world) casinoChecks()         {}
func (w *world) withdrawalChecks()     {}
func (w *world) rgChecks()             {}
func (w *world) marketChecks()         {}
func (w *world) isolationChecks()      {}
func (w *world) reconciliationChecks() {}
func (w *world) alertingChecks()       {}
func (w *world) killSwitchChecks()     {}
func (w *world) observabilityChecks()  {}
