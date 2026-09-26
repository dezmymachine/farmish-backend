package validation

import (
	"errors"
	"testing"
)

func TestError_OrNil(t *testing.T) {
	var e Error
	if err := e.OrNil(); err != nil {
		t.Errorf("empty OrNil = %v", err)
	}
	e.Add("region", "must be one of the 16 regions")
	err := e.OrNil()
	if err == nil {
		t.Fatal("OrNil = nil after Add")
	}
	var ve *Error
	if !errors.As(err, &ve) || len(ve.Fields) != 1 || ve.Fields[0].Name != "region" {
		t.Errorf("OrNil = %v", err)
	}
	var nilErr *Error
	if err := nilErr.OrNil(); err != nil {
		t.Errorf("nil OrNil = %v", err)
	}
}
