<script lang="ts">
  import ReadOnlyResponseList from "@kenn-io/roborev-ui/response-list";
  import { getReviewStores } from "../../stores/context";
  const stores = getReviewStores();
  let commentText = $state("");
  let submitting = $state(false);

  $effect(() => {
    void stores.roborevReview?.getSelectedJobId();
    commentText = "";
  });

  function handleSubmit(): void {
    const review = stores.roborevReview;
    const jobId = review?.getSelectedJobId();
    const comment = commentText.trim();
    if (!review || !jobId || !comment) return;
    submitting = true;
    review.addComment(jobId, comment, {
      onSuccess: () => {
        commentText = "";
        submitting = false;
      },
      onFailure: () => {
        submitting = false;
      },
    });
  }

  function handleKeydown(e: KeyboardEvent): void {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      handleSubmit();
    }
  }
</script>

<div class="response-list">
  {#if stores.roborevReview}
    {@const responses = stores.roborevReview.getResponses()}
    <ReadOnlyResponseList {responses} />

    {#if !stores.roborevReview.isClosed()}
      <div class="comment-input">
        <textarea
          class="comment-textarea"
          placeholder="Add a comment..."
          bind:value={commentText}
          onkeydown={handleKeydown}
          disabled={submitting}
        ></textarea>
        <button
          class="submit-btn"
          disabled={submitting || !commentText.trim()}
          onclick={handleSubmit}
        >
          {submitting ? "Sending..." : "Comment"}
        </button>
      </div>
    {/if}
  {/if}
</div>

<style>
  .response-list {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  /* Submit sits inside the field, matching the pull request and issue
   * comment boxes; the textarea reserves its footprint as bottom padding. */
  .comment-input {
    position: relative;
    padding-top: 8px;
    border-top: 1px solid var(--border-muted);
  }

  .comment-textarea {
    display: block;
    width: 100%;
    padding: 6px 10px
      calc(
        var(--focus-detail-hit-target, 39.5px) +
          var(--focus-detail-space-sm, 7.5px)
      );
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-family: inherit;
    line-height: 1.4;
    resize: vertical;
    outline: none;
    min-height: 80px;
    max-height: 200px;
  }

  .comment-textarea::placeholder {
    color: var(--text-muted);
  }

  .comment-textarea:focus {
    border-color: var(--accent-blue);
  }

  .comment-textarea:disabled {
    opacity: 0.6;
  }

  .submit-btn {
    position: absolute;
    right: var(--focus-detail-space-sm, 8px);
    bottom: var(--focus-detail-space-sm, 8px);
    padding: var(--focus-detail-space-xs, 6px)
      var(--focus-detail-space-md, 14px);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--accent-blue);
    color: var(--text-on-accent);
    font-size: var(--font-size-sm);
    font-weight: 500;
    cursor: pointer;
    white-space: nowrap;
    z-index: 1;
    transition: opacity 0.15s;
  }

  .submit-btn:hover:not(:disabled) {
    opacity: 0.85;
  }

  .submit-btn:disabled {
    opacity: 0.45;
    cursor: not-allowed;
  }
</style>
