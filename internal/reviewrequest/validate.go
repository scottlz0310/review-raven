package reviewrequest

import "regexp"

// Patterns mirror docs/design/contracts/schemas/common.schema.json.
var (
	reOwner          = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)
	reRepo           = regexp.MustCompile(`^[a-z0-9._-]{1,100}$`)
	reSHA            = regexp.MustCompile(`^[0-9a-f]{40}$`)
	reCycleID        = regexp.MustCompile(`^cyc_[0-9a-z]{26}$`)
	reCandidateID    = regexp.MustCompile(`^cand_[0-9a-z]{26}$`)
	reIdempotencyKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
	reSourceID       = regexp.MustCompile(`^[A-Za-z0-9._:/#-]{1,128}$`)
	reProducerID     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	reTimestamp      = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$`)
	reGitHubURL      = regexp.MustCompile(`^https://github\.com/[^ ]+$`)
)

func validReason(r string) bool {
	return r == "opened" || r == "synchronized" || r == "re-review-requested"
}

func validPR(owner, repo string, pr int) *Error {
	switch {
	case !reOwner.MatchString(owner):
		return newError(CodeInvalidArgument, "owner must be lowercase and match the contract pattern")
	case !reRepo.MatchString(repo):
		return newError(CodeInvalidArgument, "repo must be lowercase and match the contract pattern")
	case pr < 1:
		return newError(CodeInvalidArgument, "prNumber must be a positive integer")
	}
	return nil
}

func validateRequest(in RequestReviewInput) *Error {
	if err := validPR(in.Owner, in.Repo, in.PRNumber); err != nil {
		return err
	}
	switch {
	case !reSHA.MatchString(in.ExpectedHeadSHA):
		return newError(CodeInvalidArgument, "expectedHeadSha must be a full 40-digit lowercase hex SHA")
	case !validReason(in.Reason):
		return newError(CodeInvalidArgument, "reason must be opened, synchronized or re-review-requested")
	case !reIdempotencyKey.MatchString(in.IdempotencyKey):
		return newError(CodeInvalidArgument, "idempotencyKey must match the contract pattern")
	}
	return nil
}

func validateIngest(in IngestInput) *Error {
	if err := validPR(in.Owner, in.Repo, in.PRNumber); err != nil {
		return err
	}
	switch {
	case !reSourceID.MatchString(in.SourceID):
		return newError(CodeInvalidArgument, "sourceId must match the contract pattern")
	case !reSHA.MatchString(in.HeadSHA):
		return newError(CodeInvalidArgument, "headSha must be a full 40-digit lowercase hex SHA")
	case !validReason(in.Reason):
		return newError(CodeInvalidArgument, "reason must be opened, synchronized or re-review-requested")
	case in.Delivery != "webhook" && in.Delivery != "comment":
		return newError(CodeInvalidArgument, "delivery must be webhook or comment")
	}
	return nil
}

func validateCancel(in CancelReviewInput) *Error {
	switch {
	case !reCycleID.MatchString(in.CycleID):
		return newError(CodeInvalidArgument, "cycleId must match the contract pattern")
	case !reSHA.MatchString(in.ExpectedHeadSHA):
		return newError(CodeInvalidArgument, "expectedHeadSha must be a full 40-digit lowercase hex SHA")
	case in.ExpectedSnapshotRevision < 1:
		return newError(CodeInvalidArgument, "expectedSnapshotRevision must be at least 1")
	case !reIdempotencyKey.MatchString(in.IdempotencyKey):
		return newError(CodeInvalidArgument, "idempotencyKey must match the contract pattern")
	}
	return nil
}

func validateResult(in ResultInput) *Error {
	switch {
	case !reCycleID.MatchString(in.CycleID):
		return newError(CodeInvalidArgument, "cycleId must match the contract pattern")
	case !reCandidateID.MatchString(in.CandidateID):
		return newError(CodeInvalidArgument, "candidateId must match the contract pattern")
	case !reSHA.MatchString(in.HeadSHA):
		return newError(CodeInvalidArgument, "headSha must be a full 40-digit lowercase hex SHA")
	case in.ResultVersion < 1:
		return newError(CodeInvalidArgument, "resultVersion must be at least 1")
	case in.Kind != "reviewed" && in.Kind != "approved":
		return newError(CodeInvalidArgument, "kind must be reviewed or approved")
	case in.CommentID < 1 || !reGitHubURL.MatchString(in.URL):
		return newError(CodeInvalidArgument, "post must reference a GitHub comment")
	case !reTimestamp.MatchString(in.PostedAt):
		return newError(CodeInvalidArgument, "postedAt must be a UTC timestamp")
	}
	return nil
}

func validateObservation(in ObservationInput) *Error {
	switch {
	case !reCycleID.MatchString(in.CycleID):
		return newError(CodeInvalidArgument, "cycleId must match the contract pattern")
	case !reCandidateID.MatchString(in.CandidateID):
		return newError(CodeInvalidArgument, "candidateId must match the contract pattern")
	case !reSHA.MatchString(in.HeadSHA):
		return newError(CodeInvalidArgument, "headSha must be a full 40-digit lowercase hex SHA")
	case !reProducerID.MatchString(in.ProducerID):
		return newError(CodeInvalidArgument, "producerId must match the contract pattern")
	case in.ProducerEpoch < 1 || in.Sequence < 0:
		return newError(CodeInvalidArgument, "producerEpoch must be at least 1 and sequence at least 0")
	case !reTimestamp.MatchString(in.ObservedAt):
		return newError(CodeInvalidArgument, "observedAt must be a UTC timestamp")
	}

	// holdReason is only meaningful while waiting; outcome / exitCode only once finished.
	switch in.State {
	case ExecWaiting:
		if in.Outcome != nil || in.ExitCode != nil {
			return newError(CodeInvalidArgument, "outcome and exitCode are only valid when finished")
		}
		if in.HoldReason != nil && !validHoldReason(*in.HoldReason) {
			return newError(CodeInvalidArgument, "unknown holdReason")
		}
	case ExecRunning, ExecFinished:
		if in.HoldReason != nil {
			return newError(CodeInvalidArgument, "holdReason is only valid while waiting")
		}
		if in.State == ExecRunning && (in.Outcome != nil || in.ExitCode != nil) {
			return newError(CodeInvalidArgument, "outcome and exitCode are only valid when finished")
		}
		if in.Outcome != nil && *in.Outcome != "completed" && *in.Outcome != "failed" {
			return newError(CodeInvalidArgument, "unknown outcome")
		}
	default:
		return newError(CodeInvalidArgument, "state must be waiting, running or finished")
	}
	return nil
}

func validHoldReason(r string) bool {
	switch r {
	case "ciPending", "busy", "manual", "autoPause":
		return true
	}
	return false
}
