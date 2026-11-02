package integration

import "errors"

// ToolErrorDetails lets a host retain its existing error and audit conventions.
func ToolErrorDetails(err error) (code, message string, ok bool) {
	var detailed detailedToolError
	if errors.As(err, &detailed) {
		return string(detailed.code), detailed.message, true
	}
	var stable stableToolError
	if errors.As(err, &stable) {
		return string(stable), "", true
	}
	return "", "", false
}

// ToolError exposes only stable errors and safe, actionable backend refusals.
func ToolError(err error) error {
	if err == nil {
		return nil
	}
	err = FoundryError(err)
	if _, _, ok := ToolErrorDetails(err); ok {
		return err
	}
	return stableErrorCode(err)
}
