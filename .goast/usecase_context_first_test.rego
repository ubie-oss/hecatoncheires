package goast

# Unit tests for usecase_context_first.rego (adjacent). Self-contained:
# fixtures are supplied via `with input as`.
# Run with:  opa test .goast

# --- fixtures ---------------------------------------------------------------

# A FuncDecl named `name` under pkg/usecase whose parameter list is `params`.
func_decl(name, params) := {
	"Path": "pkg/usecase/x.go",
	"FileName": "x.go",
	"DirName": "pkg/usecase",
	"Kind": "FuncDecl",
	"Node": {
		"Name": {"Name": name, "NamePos": 200},
		"Type": {"Params": {"List": params}},
	},
}

# A parameter whose type is the selector expression context.Context.
ctx_param := {"Type": {"X": {"Name": "context"}, "Sel": {"Name": "Context"}}}

# A parameter whose type is a plain identifier (e.g. string).
str_param := {"Type": {"Name": "string"}}

# --- flagged cases ----------------------------------------------------------

test_exported_no_ctx_flagged if {
	some res in fail with input as func_decl("DoWork", [str_param])
	res.pos == 200
	res.sev == "ERROR"
	res.msg == "exported use-case function \"DoWork\" must take context.Context as its first parameter"
}

test_exported_no_params_flagged if {
	count(fail) == 1 with input as func_decl("DoWork", [])
}

# ctx present but not first — still flagged.
test_ctx_not_first_flagged if {
	count(fail) == 1 with input as func_decl("DoWork", [str_param, ctx_param])
}

# --- allowed cases ----------------------------------------------------------

test_ctx_first_allowed if {
	count(fail) == 0 with input as func_decl("RunTask", [ctx_param, str_param])
}

test_unexported_ignored if {
	count(fail) == 0 with input as func_decl("helper", [str_param])
}

# --- exemptions -------------------------------------------------------------

test_exempt_constructor if {
	count(fail) == 0 with input as func_decl("NewThing", [str_param])
}

test_exempt_functional_option if {
	count(fail) == 0 with input as func_decl("WithTimeout", [str_param])
}

test_exempt_predicate if {
	count(fail) == 0 with input as func_decl("IsReady", [])
}

test_exempt_parser if {
	count(fail) == 0 with input as func_decl("ParseValue", [str_param])
}

test_exempt_value_method if {
	count(fail) == 0 with input as func_decl("Validate", [])
}

test_exempt_job_core_tool_builder if {
	base := func_decl("CoreTools", [str_param])
	job_builder := json.patch(base, [
		{"op": "replace", "path": "/DirName", "value": "pkg/usecase/job"},
		{"op": "replace", "path": "/FileName", "value": "runner.go"},
	])
	count(fail) == 0 with input as job_builder
	count(fail) == 1 with input as base
}

# --- scoping ----------------------------------------------------------------

test_test_file_ignored if {
	count(fail) == 0 with input as json.patch(
		func_decl("DoWork", [str_param]),
		[{"op": "replace", "path": "/FileName", "value": "x_test.go"}],
	)
}

test_outside_usecase_ignored if {
	count(fail) == 0 with input as json.patch(
		func_decl("DoWork", [str_param]),
		[{"op": "replace", "path": "/DirName", "value": "pkg/controller"}],
	)
}

# The eval harness (pkg/usecase/eval/**) is out of scope.
test_eval_harness_ignored if {
	count(fail) == 0 with input as json.patch(
		func_decl("DoWork", [str_param]),
		[{"op": "replace", "path": "/DirName", "value": "pkg/usecase/eval/report"}],
	)
}
