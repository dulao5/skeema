package linter

import (
	"github.com/skeema/skeema/internal/tengo"
)

func init() {
	RegisterRule(Rule{
		CheckerFunc:     RoutineChecker(hasRoutinesChecker),
		Name:            "has-routine",
		Description:     "Flag any use of stored procs or funcs; intended for environments that restrict their presence",
		DefaultSeverity: SeverityIgnore,
	})
}

func hasRoutinesChecker(routine *tengo.Routine, _ string, _ *tengo.Schema, _ *Options) *Note {
	return &Note{
		Summary: "Routine present",
		Message: routine.ObjectKey().String() + " found. Some environments restrict use of stored procedures and functions for reasons of scalability or operational complexity.",
	}
}
