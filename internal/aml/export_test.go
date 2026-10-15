package aml

import "context"

// RecordScreeningSkippingValidation presents the DATABASE with an input that Validate would refuse, so a test can prove that
// aml_screening_record and the table CHECKs hold the SR7-2 line on their own (defence in depth behind the Go mirror).
func (s *Service) RecordScreeningSkippingValidation(ctx context.Context, a Actor, in RecordScreeningInput) (ScreeningRecorded, error) {
	return s.recordScreening(ctx, a, in)
}
