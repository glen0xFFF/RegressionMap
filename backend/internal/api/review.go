package api

// The Muse change review in the analysis pipeline (docs/API_CONTRACT.md,
// "Muse change review (v4)", "When the review runs"). After the hypothesis
// call, the review call and then the check call run one after another under
// the review chain's own deadline. Every outcome is kept on the analysis as
// soon as it exists, so an analysis that ends before the chain does (a
// shutdown, the job deadline) still gets the review the contract gives it.

import (
	"context"
	"errors"
	"time"

	"regressionmap/internal/gitx"
	"regressionmap/internal/model"
)

// reviewMargin is how long before the job deadline the review chain stops,
// so it cannot time out the analysis. Tests shorten it.
var reviewMargin = 15 * time.Second

// commitMessages lists the commits of the change. Tests swap it.
var commitMessages = gitx.CommitMessages

// maxCommitMessages is how many commits of the change are read: the
// contract's "at most 10" commit items.
const maxCommitMessages = 10

// The contract's review messages and warnings.
const (
	msgReviewDeadline = "review stopped at the analysis deadline"
	msgCheckDeadline  = "check stopped at the analysis deadline"
	msgNoTimeLeft     = "no time was left for the review"
	msgReviewEnded    = "the analysis ended before the review finished"
	msgReviewSkipped  = "the analysis ended before the change could be reviewed"
	warnCommitsUnread = "commit messages could not be read: the evidence holds no commit items"
	warnCommitsCut    = "commit messages larger than 1 MiB: only the commits that fit were read"
)

// reviewChange runs the review chain for analysis id with the hypothesis
// call's input, and returns the review the analysis ends with. ctx is the
// job's context: the chain stops reviewMargin before its deadline. A call the
// chain's deadline cuts off gets the contract's deadline message; one that a
// cancelled job context cuts off keeps the client's message, because that
// analysis has already ended and closeReview gave it its review.
func (s *Server) reviewChange(ctx context.Context, id string, in model.Input) model.Review {
	mc := s.cfg.Model
	if !mc.Configured() {
		rv := mc.NoReview("disabled", model.MsgNotConfigured)
		s.keepReviewOutcome(id, rv)
		return rv
	}
	rctx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		end := deadline.Add(-reviewMargin)
		if !time.Now().Before(end) {
			rv := mc.NoReview("error", msgNoTimeLeft)
			s.keepReviewOutcome(id, rv)
			return rv
		}
		var cancel context.CancelFunc
		rctx, cancel = context.WithDeadline(ctx, end)
		defer cancel()
	}
	// cutOff reports that a call that ended with status was cut off by the
	// chain's deadline while the job lives. Such a call ends as an error, or
	// as invalid when the deadline cut short the body of a 200 answer, which
	// then is not a chat completion.
	cutOff := func(status string) bool {
		return (status == "error" || status == "invalid") &&
			errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	}
	rv := mc.Review(rctx, in)
	if cutOff(rv.Status) {
		rv = mc.NoReview("error", msgReviewDeadline)
	}
	s.keepReviewOutcome(id, rv)
	if rv.CheckStatus != "" {
		return rv
	}
	checked := mc.Check(rctx, in, rv)
	if cutOff(checked.CheckStatus) {
		checked = rv.EndCheck("error", msgCheckDeadline)
	}
	s.keepReviewOutcome(id, checked)
	return checked
}

// keepReviewOutcome stores rv in the job table, under s.mu, as the latest
// review outcome of analysis id while it is running. It does not save: the
// outcome is shown only once finish makes it the analysis's review. The
// stored value is replaced, never changed in place.
func (s *Server) keepReviewOutcome(id string, rv model.Review) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.jobs[id]; ok && a.Status == "running" {
		done := rv
		a.reviewDone = &done
	}
}

// closeReview is the review of an analysis that ends while its review is
// still nil: before its evidence existed, or before the review chain ended.
// finish calls it with s.mu held, so it must not panic: a server without a
// model client counts as one without a key. It returns a new value and
// never changes a.reviewDone.
func (s *Server) closeReview(a *Analysis) *model.Review {
	mc := s.cfg.Model
	if mc == nil {
		mc = &model.Client{}
	}
	var rv model.Review
	switch done := a.reviewDone; {
	case !a.reviewOwed:
		rv = mc.NoReview("skipped", msgReviewSkipped)
	case !mc.Configured():
		rv = mc.NoReview("disabled", model.MsgNotConfigured)
	case done == nil:
		rv = mc.NoReview("error", msgReviewEnded)
	case done.CheckStatus == "":
		rv = done.EndCheck("error", msgReviewEnded)
	default:
		rv = *done
	}
	return &rv
}
