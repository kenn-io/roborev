# Roborev

Roborev maintains a persistent queue of AI-assisted code reviews and the work
that produces them. These terms distinguish user-visible reviews from the jobs
and panel activity behind them.

## Language

**Logical review**:
A standalone review or a panel synthesis parent that appears as one review in
user-facing history and metrics.
_Avoid_: Review job when referring to the user-visible result

**Review job**:
One queued agent execution that can produce a review result or contribute to a
panel.
_Avoid_: Logical review, experiment

**Review panel**:
A named group of reviewers that examines one target and produces one synthesized
logical review.
_Avoid_: Batch, multi-arm review

**Panel member**:
One reviewer within a review panel.
_Avoid_: Experiment arm, sub-review

**Synthesis**:
The panel step that combines member results into the panel's logical review.
_Avoid_: Summary job, evaluator

**Review unit**:
The complete subject of one experiment assignment: either a standalone review
or an entire review panel.
_Avoid_: Review job when referring to a panel

**Experiment definition**:
A versioned description of an experiment's eligible workflows, allocation, and
experimental review configuration.
_Avoid_: Feature flag, experiment run

**Experiment assignment**:
The immutable placement of one review unit in an experiment arm.
_Avoid_: Experiment definition, eligibility

**Default arm**:
The experiment arm that uses the normal resolved review configuration.
_Avoid_: Control configuration

**Experimental arm**:
The experiment arm that applies the experiment's review-configuration overlay.
_Avoid_: Variant, treatment configuration

**Branch subject**:
A repository-scoped source branch used as the stable identity for experiment
assignment and review-session reuse.
_Avoid_: Commit, pull request
