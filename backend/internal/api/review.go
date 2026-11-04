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
		return s.reviewOutcome(id, mc.NoReview("disabled", model.MsgNotConfigured))
	}
	rctx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		end := deadline.Add(-reviewMargin)
		if !time.Now().Before(end) {
			return s.reviewOutcome(id, mc.NoReview("error", msgNoTimeLeft))
		}
		var cancel context.CancelFunc
		rctx, cancel = context.WithDeadline(ctx, end)
		defer cancel()
	}
	// cutOff reports that the chain's deadline passed while the job lives.
	cutOff := func() bool {
		return errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	}
	rv := mc.Review(rctx, in)
	if rv.Status == "error" && cutOff() {
		rv = mc.NoReview("error", msgReviewDeadline)
	}
	s.reviewOutcome(id, rv)
	if rv.CheckStatus != "" {
		return rv
	}
	checked := mc.Check(rctx, in, rv)
	if checked.CheckStatus == "error" && cutOff() {
		checked = rv.EndCheck("error", msgCheckDeadline)
	}
	return s.reviewOutcome(id, checked)
}

// reviewOutcome keeps rv as the latest review outcome of analysis id while
// it is running, and returns rv. It does not save: the outcome is shown only
// once finish makes it the analysis's review. The stored value is replaced,
// never changed in place.
func (s *Server) reviewOutcome(id string, rv model.Review) model.Review {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.jobs[id]; ok && a.Status == "running" {
		done := rv
		a.reviewDone = &done
	}
	return rv
}

// closeReview is the review of an analysis that ends while its review is
// still nil: before its evidence existed, or before the review chain ended.
// finish calls it with s.mu held. It returns a new value and never changes
// a.reviewDone.
func (s *Server) closeReview(a *Analysis) *model.Review {
	mc := s.cfg.Model
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
