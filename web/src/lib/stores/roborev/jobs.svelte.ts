import { Effect, Option } from "effect";
import type { AppRuntime } from "../../runtime/runtime";
import {
  executeRoborevRequest,
  type RoborevClient,
  RoborevStreamError,
} from "../../api/client";
import type { components, operations } from "../../api/generated";
import {
  isPanelParent,
  panelCostUsd,
  panelElapsedStart,
} from "../../utils/roborev-panel";
import {
  makeRoborevOwner,
  RoborevMutationError,
  roborevMutationFailureMessage,
  RoborevResponseError,
  RoborevWorkflow,
} from "./workflow";

type ReviewJob = components["schemas"]["ReviewJob"];
type JobStats = components["schemas"]["JobStats"];
type CancelJobResponse = components["schemas"]["CancelJobOutputBody"];
type ListJobsQuery = NonNullable<
  operations["list-jobs"]["parameters"]["query"]
>;

export interface JobsStoreOptions {
  client: RoborevClient;
  runtime: AppRuntime;
  owner: string;
  navigate: (jobId?: number) => void;
  onError?: (msg: string) => void;
}

export interface JobStatusCounts {
  queued: number;
  running: number;
  done: number;
  failed: number;
}

interface JobsAuthority {
  readonly jobs: ReadonlyArray<ReviewJob>;
  readonly hasMore: boolean;
  readonly nextCursor: string | undefined;
  readonly stats: JobStats;
  readonly filteredStatusCounts: Option.Option<JobStatusCounts>;
  readonly countScope: string;
  readonly queryScope: string;
}

type SortColumn =
  | "id"
  | "status"
  | "verdict"
  | "agent"
  | "elapsed"
  | "cost"
  | "job_type"
  | "enqueued_at";
type SortDirection = "asc" | "desc";
type StringFilterKey = "repo" | "branch" | "status" | "search" | "jobType";
type BooleanFilterKey = "hideClosed" | "showAutoDesign";
type FilterKey = StringFilterKey | BooleanFilterKey;

const HIDE_CLOSED_STORAGE_KEY = "roborev:web:hideClosed";
const SHOW_AUTO_DESIGN_STORAGE_KEY = "roborev:web:showAutoDesign";
const DEFAULT_PAGE_LIMIT = 50;
const MAX_LOADED_JOBS = 10_000;
const booleanFilterPreferences = $state<Record<BooleanFilterKey, boolean>>({
  hideClosed: false,
  showAutoDesign: false,
});
const booleanFilterSubscribers = new Map<symbol, () => void>();

function readBooleanPreference(key: string): boolean {
  try {
    return localStorage.getItem(key) === "1";
  } catch {
    return false;
  }
}

function writeBooleanPreference(key: string, value: boolean): void {
  try {
    localStorage.setItem(key, value ? "1" : "0");
  } catch {
    // Storage is best-effort; reactive state still owns this store instance.
  }
}

function notifyBooleanFilterSubscribers(origin?: symbol): void {
  for (const [owner, refresh] of booleanFilterSubscribers) {
    if (owner !== origin) refresh();
  }
}

function hydrateBooleanFilterPreferences(): void {
  const hideClosed = readBooleanPreference(HIDE_CLOSED_STORAGE_KEY);
  const showAutoDesign = readBooleanPreference(SHOW_AUTO_DESIGN_STORAGE_KEY);
  if (
    hideClosed === booleanFilterPreferences.hideClosed &&
    showAutoDesign === booleanFilterPreferences.showAutoDesign
  ) {
    return;
  }
  booleanFilterPreferences.hideClosed = hideClosed;
  booleanFilterPreferences.showAutoDesign = showAutoDesign;
  notifyBooleanFilterSubscribers();
}

function setBooleanFilterPreference(
  key: BooleanFilterKey,
  value: boolean,
  origin: symbol,
): void {
  if (booleanFilterPreferences[key] === value) return;
  booleanFilterPreferences[key] = value;
  writeBooleanPreference(
    key === "hideClosed"
      ? HIDE_CLOSED_STORAGE_KEY
      : SHOW_AUTO_DESIGN_STORAGE_KEY,
    value,
  );
  notifyBooleanFilterSubscribers(origin);
}

export function createJobsStore(opts: JobsStoreOptions) {
  const client = opts.client;
  const filterOwner = Symbol(opts.owner);
  let disposed = false;

  if (booleanFilterSubscribers.size === 0) hydrateBooleanFilterPreferences();

  // State
  let jobs = $state<ReviewJob[]>([]);
  let loading = $state(false);
  let hasMore = $state(false);
  let allResultsLoaded = $state(true);
  let nextCursor: string | undefined;
  let stats = $state<JobStats>({
    queued: 0,
    running: 0,
    done: 0,
    failed: 0,
    canceled: 0,
    skipped: 0,
    closed: 0,
    open: 0,
  });
  let filteredStatusCounts = $state<JobStatusCounts | undefined>(undefined);
  let filteredStatusCountsScope: string | undefined;
  let storeError = $state<string | null>(null);
  let selectedJobId = $state<number | undefined>(undefined);
  let selectedReviewRevision = $state(0);
  let highlightedJobId = $state<number | undefined>(undefined);
  let rerunningJobIds = $state<Set<number>>(new Set());
  let loadedLimit = DEFAULT_PAGE_LIMIT;
  let jobsAuthorityGeneration = 0;

  // Filters
  let filterRepo = $state<string | undefined>(undefined);
  let filterBranch = $state<string | undefined>(undefined);
  let filterStatus = $state<string | undefined>(undefined);
  let filterSearch = $state<string | undefined>(undefined);
  let filterJobType = $state<string | undefined>(undefined);

  // Sorting (client-side)
  let sortColumn = $state<SortColumn>("enqueued_at");
  let sortDirection = $state<SortDirection>("desc");

  // Panel expansion, keyed by panel_run_uuid. Member lists are cached per
  // run and refreshed alongside the listing so SSE-driven reloads keep
  // expanded panels live.
  let expandedPanels = $state<Record<string, boolean>>({});
  let panelMembers = $state<Record<string, ReviewJob[]>>({});
  let panelMemberErrors = $state<Record<string, string>>({});
  let loadingMembers = $state<Record<string, boolean>>({});
  let interestedPanelRun: string | undefined = undefined;

  // Roborev streams newline-delimited JSON, not server-sent events.
  let eventStreamConnected = $state(false);
  let activeEventOwner: string | undefined;

  function buildQuery(): ListJobsQuery {
    const q: ListJobsQuery = { limit: loadedLimit, omit_prompt: "true" };
    if (filterRepo) q.repo = [filterRepo];
    if (filterBranch === "(none)") q.branch_empty = "true";
    else if (filterBranch) q.branch = filterBranch;
    if (filterStatus) q.status = filterStatus;
    if (filterSearch) q.git_ref = filterSearch;
    if (booleanFilterPreferences.hideClosed) q.closed = "false";
    if (filterJobType) q.job_type = filterJobType;
    if (!booleanFilterPreferences.showAutoDesign) q.hide_classify_jobs = "true";
    return q;
  }

  function filterScope(): string {
    const query = buildQuery();
    delete query.limit;
    return JSON.stringify(query);
  }

  function queryHasActiveFilters(query: ListJobsQuery): boolean {
    return Boolean(
      query.repo ||
      query.branch ||
      query.branch_empty ||
      query.status ||
      query.git_ref ||
      query.closed ||
      query.job_type ||
      query.hide_classify_jobs,
    );
  }

  function hasActiveFilters(): boolean {
    return queryHasActiveFilters(buildQuery());
  }

  function statusCountsFromStats(stats: JobStats): JobStatusCounts {
    return {
      queued: stats.queued ?? 0,
      running: stats.running ?? 0,
      done: stats.done,
      failed: stats.failed ?? 0,
    };
  }

  function getElapsedSeconds(job: ReviewJob): number {
    const startedAt = panelElapsedStart(job, getPanelMembersForJob(job));
    if (!startedAt) return -1;
    const start = new Date(startedAt).getTime();
    const end = job.finished_at
      ? new Date(job.finished_at).getTime()
      : Date.now();
    return Math.max(0, Math.floor((end - start) / 1000));
  }

  function getPanelMembersForJob(job: ReviewJob): ReviewJob[] | undefined {
    const runUuid = job.panel_run_uuid;
    return runUuid ? panelMembers[runUuid] : undefined;
  }

  function getPanelParentForMemberId(memberId: number): ReviewJob | undefined {
    for (const job of jobs) {
      const runUuid = job.panel_run_uuid;
      if (!runUuid || !isPanelParent(job)) continue;
      if (panelMembers[runUuid]?.some((member) => member.id === memberId)) {
        return job;
      }
    }
    return undefined;
  }

  function wantsPanelMembers(runUuid: string): boolean {
    return expandedPanels[runUuid] === true || interestedPanelRun === runUuid;
  }

  function getSortValue(job: ReviewJob, col: SortColumn): string | number {
    switch (col) {
      case "id":
        return job.id;
      case "status":
        return job.status;
      case "verdict":
        return job.verdict ?? "";
      case "agent":
        return job.agent;
      case "elapsed":
        return getElapsedSeconds(job);
      case "cost":
        return panelCostUsd(job, getPanelMembersForJob(job)) ?? -1;
      case "job_type":
        return job.job_type;
      case "enqueued_at":
        return job.enqueued_at;
      default:
        return job.id;
    }
  }

  function sortJobs(list: ReviewJob[]): ReviewJob[] {
    // The daemon already returns this order using its cursor-compatible,
    // normalized timestamp expression. Preserve it exactly, including for
    // legacy rows whose timestamp text uses a different format.
    if (sortColumn === "enqueued_at" && sortDirection === "desc") {
      return [...list];
    }
    const dir = sortDirection === "asc" ? 1 : -1;
    return [...list].sort((a, b) => {
      const av = getSortValue(a, sortColumn);
      const bv = getSortValue(b, sortColumn);
      if (av < bv) return -1 * dir;
      if (av > bv) return 1 * dir;
      return 0;
    });
  }

  const fetchJobsAuthority = Effect.fn("RoborevJobs.fetchAuthority")(function* (
    query: ListJobsQuery,
  ) {
    const filtered = queryHasActiveFilters(query);
    const countScope = JSON.stringify(query);
    const result = yield* executeRoborevRequest("list Roborev jobs", (signal) =>
      client.GET("/api/jobs", { params: { query }, signal }),
    );
    if (result.error !== undefined) {
      return yield* Effect.fail(
        RoborevResponseError.make({
          operation: "list Roborev jobs",
          message: "Failed to load jobs",
          cause: result.error,
        }),
      );
    }
    const stats = result.data?.stats ?? {
      queued: 0,
      running: 0,
      done: 0,
      failed: 0,
      canceled: 0,
      skipped: 0,
      closed: 0,
      open: 0,
    };
    const exactStats = result.data?.filtered_stats ?? stats;
    return {
      jobs: result.data?.jobs ?? [],
      hasMore: result.data?.has_more ?? false,
      nextCursor: result.data?.next_cursor ?? undefined,
      stats,
      filteredStatusCounts: filtered
        ? Option.some(statusCountsFromStats(exactStats))
        : Option.none(),
      countScope,
      queryScope: JSON.stringify(query),
    } satisfies JobsAuthority;
  });

  const publishJobsAuthority = (authority: JobsAuthority, generation: number) =>
    Effect.sync(() => {
      if (generation !== jobsAuthorityGeneration) return [];
      if (JSON.stringify(buildQuery()) !== authority.queryScope) return [];
      if (authority.hasMore) {
        sortColumn = "enqueued_at";
        sortDirection = "desc";
      }
      jobs = sortJobs([...authority.jobs]);
      hasMore = authority.hasMore && jobs.length < MAX_LOADED_JOBS;
      allResultsLoaded = !authority.hasMore;
      nextCursor = authority.nextCursor;
      stats = authority.stats;
      if (Option.isSome(authority.filteredStatusCounts)) {
        filteredStatusCounts = authority.filteredStatusCounts.value;
        filteredStatusCountsScope = authority.countScope;
      }
      const runs = new Set<string>();
      for (const job of jobs) {
        const runUuid = job.panel_run_uuid;
        if (runUuid && wantsPanelMembers(runUuid)) runs.add(runUuid);
      }
      if (interestedPanelRun) runs.add(interestedPanelRun);
      adjustHiddenHighlight();
      return Array.from(runs);
    });

  const refreshJobsAuthority = (query: ListJobsQuery) =>
    Effect.gen(function* () {
      const generation = yield* Effect.sync(() => {
        jobsAuthorityGeneration += 1;
        return jobsAuthorityGeneration;
      });
      const authority = yield* fetchJobsAuthority(query);
      const expandedRuns = yield* publishJobsAuthority(authority, generation);
      yield* Effect.forEach(expandedRuns, fetchPanelMembersEffect, {
        discard: true,
      });
      return authority;
    });

  const loadJobsRequestEffect = (requestOwner = opts.owner) =>
    Effect.gen(function* () {
      const workflow = yield* RoborevWorkflow;
      const query = buildQuery();
      return yield* workflow.jobs(
        requestOwner,
        Effect.sync(() => {
          loading = true;
          storeError = null;
          const countScope = JSON.stringify(query);
          if (hasActiveFilters() && filteredStatusCountsScope !== countScope)
            filteredStatusCounts = undefined;
        }).pipe(
          Effect.andThen(refreshJobsAuthority(query)),
          Effect.asVoid,
          Effect.ensuring(
            Effect.sync(() => {
              loading = false;
            }),
          ),
        ),
      );
    });

  const loadJobsEffect = () =>
    loadJobsRequestEffect().pipe(
      Effect.catch((failure) =>
        Effect.sync(() => {
          storeError =
            failure instanceof Error ? failure.message : String(failure);
        }),
      ),
    );

  function loadJobs(): void {
    opts.runtime.runCommand(loadJobsEffect(), {
      operation: "load Roborev jobs",
      safeContext: { owner: opts.owner },
      onFailure: () => {},
    });
  }

  const loadMoreEffect = () =>
    Effect.gen(function* () {
      if (!hasMore || loading || jobs.length === 0) return;
      if (jobs.length >= MAX_LOADED_JOBS) {
        hasMore = false;
        return;
      }
      const workflow = yield* RoborevWorkflow;
      const query = buildQuery();
      query.limit = DEFAULT_PAGE_LIMIT;
      if (nextCursor) {
        query.cursor = nextCursor;
      } else {
        query.before = jobs.reduce((oldest, candidate) => {
          const candidateTime = Date.parse(candidate.enqueued_at);
          const oldestTime = Date.parse(oldest.enqueued_at);
          return candidateTime < oldestTime ||
            (candidateTime === oldestTime && candidate.id < oldest.id)
            ? candidate
            : oldest;
        }).id;
      }
      yield* workflow.jobs(
        opts.owner,
        Effect.gen(function* () {
          yield* Effect.sync(() => {
            loading = true;
            storeError = null;
          });
          const result = yield* executeRoborevRequest(
            "load more Roborev jobs",
            (signal) => client.GET("/api/jobs", { params: { query }, signal }),
          );
          if (result.error) {
            return yield* Effect.fail(
              RoborevResponseError.make({
                operation: "load more Roborev jobs",
                message: "Failed to load more jobs",
                cause: result.error,
              }),
            );
          }
          yield* Effect.sync(() => {
            const existingIds = new Set(jobs.map((job) => job.id));
            const remaining = MAX_LOADED_JOBS - jobs.length;
            const fresh = (result.data?.jobs ?? [])
              .filter((job) => !existingIds.has(job.id))
              .slice(0, remaining);
            jobs = sortJobs([...jobs, ...fresh]);
            hasMore =
              (result.data?.has_more ?? false) && jobs.length < MAX_LOADED_JOBS;
            allResultsLoaded = !(result.data?.has_more ?? false);
            nextCursor = result.data?.next_cursor ?? undefined;
            loadedLimit = Math.max(DEFAULT_PAGE_LIMIT, jobs.length);
          });
        }).pipe(
          Effect.catch((failure) =>
            Effect.sync(() => {
              storeError =
                failure instanceof Error ? failure.message : String(failure);
            }),
          ),
          Effect.ensuring(
            Effect.sync(() => {
              loading = false;
            }),
          ),
        ),
      );
    });

  function loadMore(): void {
    opts.runtime.runCommand(loadMoreEffect(), {
      operation: "load more Roborev jobs",
      safeContext: { owner: opts.owner },
      onFailure: () => {},
    });
  }

  // Filter actions
  function setFilter(key: StringFilterKey, value: string | undefined): void;
  function setFilter(key: BooleanFilterKey, value: boolean): void;
  function setFilter(
    key: FilterKey,
    value: string | boolean | undefined,
  ): void {
    const previousScope = filterScope();
    switch (key) {
      case "repo":
        if (typeof value === "boolean") return;
        filterRepo = value;
        break;
      case "branch":
        if (typeof value === "boolean") return;
        filterBranch = value;
        break;
      case "status":
        if (typeof value === "boolean") return;
        filterStatus = value;
        break;
      case "search":
        if (typeof value === "boolean") return;
        filterSearch = value;
        break;
      case "hideClosed":
        if (typeof value !== "boolean") return;
        setBooleanFilterPreference(key, value, filterOwner);
        break;
      case "jobType":
        if (typeof value === "boolean") return;
        filterJobType = value;
        break;
      case "showAutoDesign":
        if (typeof value !== "boolean") return;
        setBooleanFilterPreference(key, value, filterOwner);
        break;
    }
    if (filterScope() !== previousScope) loadedLimit = DEFAULT_PAGE_LIMIT;
    loadJobs();
  }

  function setRepoBranchFilter(
    repo: string | undefined,
    branch: string | undefined,
  ): void {
    const previousScope = filterScope();
    filterRepo = repo;
    filterBranch = branch;
    if (filterScope() !== previousScope) loadedLimit = DEFAULT_PAGE_LIMIT;
    loadJobs();
  }

  function setSortColumn(col: SortColumn): void {
    if (!canSortJobs()) return;
    if (sortColumn === col) {
      sortDirection = sortDirection === "asc" ? "desc" : "asc";
    } else {
      sortColumn = col;
      sortDirection = col === "id" ? "desc" : "asc";
    }
    jobs = sortJobs(jobs);
  }

  // Job actions
  const fetchJobAuthority = Effect.fn("RoborevJobs.fetchJobAuthority")(
    function* (id: number) {
      const result = yield* executeRoborevRequest(
        "load authoritative Roborev job",
        (signal) =>
          client.GET("/api/jobs", {
            params: {
              query: {
                id,
                limit: 1,
                omit_prompt: "true",
              } satisfies ListJobsQuery,
            },
            signal,
          }),
      );
      if (result.error !== undefined) {
        return yield* Effect.fail(
          RoborevResponseError.make({
            operation: "load authoritative Roborev job",
            message: "Failed to revalidate job",
            cause: result.error,
          }),
        );
      }
      return result.data?.jobs?.[0];
    },
  );

  const reconcileJobMutation = <A>(
    id: number,
    query: ListJobsQuery,
    acknowledged: Option.Option<A>,
    observedValue: A,
    isApplied: (job: ReviewJob) => boolean,
  ) =>
    Effect.all(
      {
        target: fetchJobAuthority(id),
        page: refreshJobsAuthority(query),
      },
      { concurrency: "unbounded" },
    ).pipe(
      Effect.map(({ target }) =>
        target !== undefined && isApplied(target)
          ? Option.some(Option.getOrElse(acknowledged, () => observedValue))
          : Option.none<A>(),
      ),
    );

  const cancelJobEffect = (id: number) =>
    Effect.gen(function* () {
      const workflow = yield* RoborevWorkflow;
      const query = buildQuery();
      const observedValue = { success: true } satisfies CancelJobResponse;
      const response = yield* workflow.mutate({
        key: `job:${id}`,
        operation: "cancel Roborev job",
        mutation: executeRoborevRequest("cancel Roborev job", (signal) =>
          client.POST("/api/job/cancel", {
            body: { job_id: id },
            signal,
          }),
        ).pipe(
          Effect.flatMap((result) =>
            result.error || !result.data
              ? Effect.fail(
                  RoborevMutationError.make({
                    operation: "cancel Roborev job",
                    cause:
                      result.error ??
                      new Error("Roborev cancellation response was empty"),
                  }),
                )
              : Effect.succeed(result.data),
          ),
        ),
        reconcile: (acknowledged) =>
          reconcileJobMutation(
            id,
            query,
            acknowledged,
            observedValue,
            (job) => job.status === "canceled",
          ),
        onAcknowledgedRefreshFailure: () =>
          Effect.sync(() =>
            opts.onError?.(
              "Job was canceled, but the refreshed job list is unavailable",
            ),
          ),
      });
      yield* Effect.sync(() => {
        if (selectedJobId === id) selectedReviewRevision += 1;
      });
      return response;
    });

  function cancelJob(id: number): void {
    opts.runtime.runCommand(cancelJobEffect(id), {
      operation: "cancel Roborev job",
      safeContext: { job_id: id, owner: opts.owner },
      onFailure: (failure) =>
        opts.onError?.(
          roborevMutationFailureMessage("Failed to cancel job", failure),
        ),
    });
  }

  const rerunJobEffect = (id: number) =>
    Effect.gen(function* () {
      const workflow = yield* RoborevWorkflow;
      const query = buildQuery();
      const requestID = globalThis.crypto.randomUUID();
      const submit = executeRoborevRequest("rerun Roborev job", (signal) =>
        client.POST("/api/job/rerun", {
          body: { job_id: id, request_id: requestID },
          signal,
        }),
      ).pipe(
        Effect.flatMap((result) =>
          result.error || !result.data
            ? Effect.fail(
                RoborevMutationError.make({
                  operation: "rerun Roborev job",
                  cause:
                    result.error ??
                    new Error("Roborev rerun response was empty"),
                }),
              )
            : Effect.succeed(result.data),
        ),
      );
      const response = yield* workflow.mutate({
        key: `job:${id}`,
        operation: "rerun Roborev job",
        mutation: submit,
        reconcile: (acknowledged) =>
          Option.match(acknowledged, {
            onNone: () => submit,
            onSome: Effect.succeed,
          }).pipe(
            Effect.flatMap((result) =>
              reconcileJobMutation(
                result.job_id,
                query,
                Option.some(result),
                result,
                () => true,
              ),
            ),
          ),
        onAcknowledgedRefreshFailure: () =>
          Effect.sync(() =>
            opts.onError?.(
              "Job was rerun, but the refreshed job list is unavailable",
            ),
          ),
      });
      yield* Effect.sync(() => {
        if (selectedJobId === id) selectedReviewRevision += 1;
      });
      if (response.job_id !== id) {
        yield* Effect.sync(() => opts.navigate(response.job_id));
      }
      return response;
    });

  function rerunJob(id: number): void {
    if (rerunningJobIds.has(id)) return;
    rerunningJobIds = new Set(rerunningJobIds).add(id);
    const rerun = rerunJobEffect(id).pipe(
      Effect.ensuring(
        Effect.sync(() => {
          const next = new Set(rerunningJobIds);
          next.delete(id);
          rerunningJobIds = next;
        }),
      ),
    );
    opts.runtime.runCommand(rerun, {
      operation: "rerun Roborev job",
      safeContext: { job_id: id, owner: opts.owner },
      onFailure: (failure) =>
        opts.onError?.(
          roborevMutationFailureMessage("Failed to rerun job", failure),
        ),
    });
  }

  function isRerunning(id: number): boolean {
    return rerunningJobIds.has(id);
  }

  const fetchPanelMembersEffect = (runUuid: string) =>
    Effect.gen(function* () {
      const workflow = yield* RoborevWorkflow;
      yield* workflow.panel(opts.owner, runUuid, {
        onStart: Effect.sync(() => {
          loadingMembers = { ...loadingMembers, [runUuid]: true };
          const startedErrors = { ...panelMemberErrors };
          delete startedErrors[runUuid];
          panelMemberErrors = startedErrors;
        }),
        read: executeRoborevRequest("load Roborev panel members", (signal) =>
          client.GET("/api/jobs", {
            params: {
              query: { panel_run: runUuid, limit: 0, omit_prompt: "true" },
            },
            signal,
          }),
        ).pipe(
          Effect.flatMap((result) =>
            result.error
              ? Effect.fail(
                  RoborevResponseError.make({
                    operation: "load Roborev panel members",
                    message: "Failed to load panel members",
                    cause: result.error,
                  }),
                )
              : Effect.succeed(
                  (result.data?.jobs ?? [])
                    .filter((job) => job.panel_role === "member")
                    .sort(
                      (a, b) =>
                        (a.panel_member_index ?? 0) -
                        (b.panel_member_index ?? 0),
                    ),
                ),
          ),
        ),
        onSuccess: (members) =>
          Effect.sync(() => {
            panelMembers = { ...panelMembers, [runUuid]: members };
            const memberErrors = { ...panelMemberErrors };
            delete memberErrors[runUuid];
            panelMemberErrors = memberErrors;
            adjustHiddenHighlight(runUuid);
            if (sortColumn === "cost" || sortColumn === "elapsed")
              jobs = sortJobs(jobs);
          }),
        onFailure: (failure) =>
          Effect.sync(() => {
            const message =
              failure instanceof Error ? failure.message : String(failure);
            panelMemberErrors = { ...panelMemberErrors, [runUuid]: message };
            opts.onError?.(message);
          }),
        onSettled: Effect.sync(() => {
          loadingMembers = { ...loadingMembers, [runUuid]: false };
        }),
      });
    });

  function fetchPanelMembers(runUuid: string): void {
    opts.runtime.runCommand(fetchPanelMembersEffect(runUuid), {
      operation: "load Roborev panel members",
      safeContext: { owner: opts.owner },
      onFailure: () => {},
    });
  }

  function togglePanel(job: ReviewJob): void {
    if (!isPanelParent(job)) return;
    const runUuid = job.panel_run_uuid;
    if (!runUuid) return;
    const open = expandedPanels[runUuid] === true;
    if (open && highlightedJobId !== undefined) {
      const highlightedMember =
        panelMembers[runUuid]?.some(
          (member) => member.id === highlightedJobId,
        ) ?? false;
      if (highlightedMember) highlightedJobId = job.id;
    }
    expandedPanels = { ...expandedPanels, [runUuid]: !open };
    if (
      !open &&
      panelMembers[runUuid] === undefined &&
      loadingMembers[runUuid] !== true
    ) {
      fetchPanelMembers(runUuid);
    }
  }

  function ensurePanelMembers(runUuid: string): void {
    if (
      panelMembers[runUuid] === undefined &&
      loadingMembers[runUuid] !== true
    ) {
      fetchPanelMembers(runUuid);
    }
  }

  function setPanelMemberInterest(runUuid: string | undefined): void {
    interestedPanelRun = runUuid;
    if (runUuid !== undefined) fetchPanelMembers(runUuid);
  }

  function refreshPanelMembers(runUuid: string): void {
    fetchPanelMembers(runUuid);
  }

  function adjustHiddenHighlight(runUuid?: string): void {
    if (highlightedJobId === undefined) return;
    if (getVisibleJobs().some((job) => job.id === highlightedJobId)) return;
    const parent =
      runUuid !== undefined
        ? jobs.find(
            (job) => isPanelParent(job) && job.panel_run_uuid === runUuid,
          )
        : getPanelParentForMemberId(highlightedJobId);
    highlightedJobId = parent?.id;
  }

  function isPanelExpanded(runUuid: string): boolean {
    return expandedPanels[runUuid] === true;
  }

  function getPanelMembers(runUuid: string): ReviewJob[] | undefined {
    return panelMembers[runUuid];
  }

  function getPanelMemberError(runUuid: string): string | undefined {
    return panelMemberErrors[runUuid];
  }

  function isLoadingMembers(runUuid: string): boolean {
    return loadingMembers[runUuid] === true;
  }

  // Selection — setSelectedJobId sets state only (no
  // navigation), used by the route-sync effect to avoid
  // an infinite effect_update_depth_exceeded cycle.
  function setSelectedJobId(id: number | undefined): void {
    selectedJobId = id;
  }

  function selectJob(id: number): void {
    selectedJobId = id;
    highlightedJobId = id;
    opts.navigate(id);
  }

  function deselectJob(): void {
    selectedJobId = undefined;
    opts.navigate();
  }

  const connectEventStreamEffect = (baseUrl: string, eventOwner: string) =>
    Effect.gen(function* () {
      const workflow = yield* RoborevWorkflow;
      const liveReconcileOwner = `${eventOwner}:reconcile`;
      // Stream reconciliation has independent latest-request ownership. UI
      // filtering, pagination, and refreshes intentionally cancel one another,
      // but must never interrupt the stream callback that keeps live state in
      // sync.
      const reconcile = (owner: string) =>
        loadJobsRequestEffect(owner).pipe(
          Effect.provideService(RoborevWorkflow, workflow),
          Effect.mapError((cause) =>
            RoborevStreamError.make({
              operation: "reconcile Roborev jobs from live events",
              retryable: true,
              cause,
            }),
          ),
        );
      yield* workflow.connectEvents({
        owner: eventOwner,
        baseUrl,
        onInitialOpen: reconcile(eventOwner),
        onOpen: Effect.sync(() => {
          if (activeEventOwner !== eventOwner) return;
          eventStreamConnected = true;
        }),
        onEvent: (event) =>
          Effect.sync(() => {
            if (
              activeEventOwner === eventOwner &&
              selectedJobId !== undefined &&
              (selectedJobId === event.job_id ||
                event.type === "review.commented")
            ) {
              selectedReviewRevision += 1;
            }
          }),
        onEventReconcile: reconcile(liveReconcileOwner),
        onReconnect: () =>
          Effect.sync(() => {
            if (
              activeEventOwner === eventOwner &&
              selectedJobId !== undefined
            ) {
              // The connection may have missed any job or review-state event
              // while it was down, so revalidate the selected authority once
              // before resuming the stream.
              selectedReviewRevision += 1;
            }
          }).pipe(Effect.andThen(reconcile(eventOwner))),
        onError: () =>
          Effect.sync(() => {
            if (activeEventOwner !== eventOwner) return;
            eventStreamConnected = false;
          }),
      });
    });

  function connectEventStream(baseUrl: string): string {
    const eventOwner = makeRoborevOwner(`${opts.owner}:events`);
    activeEventOwner = eventOwner;
    eventStreamConnected = false;
    opts.runtime.runCommand(connectEventStreamEffect(baseUrl, eventOwner), {
      operation: "connect Roborev event stream",
      safeContext: { owner: eventOwner },
      onFailure: () => {
        if (activeEventOwner !== eventOwner) return;
        eventStreamConnected = false;
      },
    });
    return eventOwner;
  }

  const disconnectEventStreamEffect = (eventOwner: string) =>
    Effect.gen(function* () {
      const workflow = yield* RoborevWorkflow;
      yield* workflow.stop(eventOwner);
      yield* workflow.stop(`${eventOwner}:reconcile`);
      yield* Effect.sync(() => {
        if (activeEventOwner !== eventOwner) return;
        activeEventOwner = undefined;
        eventStreamConnected = false;
      });
    });

  function disconnectEventStream(eventOwner: string): void {
    opts.runtime.runCommand(disconnectEventStreamEffect(eventOwner), {
      operation: "disconnect Roborev event stream",
      safeContext: { owner: eventOwner },
      onFailure: () => {},
    });
  }

  // Selection helpers for keyboard nav
  function selectNextJob(): void {
    const visibleJobs = getVisibleJobs();
    if (visibleJobs.length === 0) return;
    if (selectedJobId === undefined) {
      selectJob(visibleJobs[0]!.id);
      return;
    }
    const idx = visibleJobs.findIndex((j) => j.id === selectedJobId);
    if (idx < visibleJobs.length - 1) {
      selectJob(visibleJobs[idx + 1]!.id);
    }
  }

  function selectPrevJob(): void {
    const visibleJobs = getVisibleJobs();
    if (visibleJobs.length === 0) return;
    if (selectedJobId === undefined) {
      selectJob(visibleJobs[visibleJobs.length - 1]!.id);
      return;
    }
    const idx = visibleJobs.findIndex((j) => j.id === selectedJobId);
    if (idx > 0) {
      selectJob(visibleJobs[idx - 1]!.id);
    }
  }

  function getVisibleJobs(): ReviewJob[] {
    const visible: ReviewJob[] = [];
    for (const job of jobs) {
      visible.push(job);
      const runUuid = job.panel_run_uuid;
      if (
        runUuid &&
        expandedPanels[runUuid] === true &&
        panelMembers[runUuid] !== undefined
      ) {
        visible.push(...(panelMembers[runUuid] ?? []));
      }
    }
    return visible;
  }

  // Highlight navigation (j/k without opening drawer)
  function highlightJob(id: number): void {
    highlightedJobId = id;
  }

  function highlightNextJob(): void {
    const visibleJobs = getVisibleJobs();
    if (visibleJobs.length === 0) return;
    if (highlightedJobId === undefined) {
      highlightedJobId = visibleJobs[0]!.id;
      return;
    }
    const idx = visibleJobs.findIndex((j) => j.id === highlightedJobId);
    if (idx < visibleJobs.length - 1) {
      highlightedJobId = visibleJobs[idx + 1]!.id;
    }
  }

  function highlightPrevJob(): void {
    const visibleJobs = getVisibleJobs();
    if (visibleJobs.length === 0) return;
    if (highlightedJobId === undefined) {
      highlightedJobId = visibleJobs[visibleJobs.length - 1]!.id;
      return;
    }
    const idx = visibleJobs.findIndex((j) => j.id === highlightedJobId);
    if (idx > 0) {
      highlightedJobId = visibleJobs[idx - 1]!.id;
    }
  }

  // Getters
  function getJobs(): ReviewJob[] {
    return jobs;
  }
  function isLoading(): boolean {
    return loading;
  }
  function getHasMore(): boolean {
    return hasMore;
  }
  function getStats(): JobStats {
    return stats;
  }
  function getFilteredStatusCounts(): JobStatusCounts | undefined {
    return filteredStatusCounts;
  }
  function usesFilteredStatusCounts(): boolean {
    return hasActiveFilters();
  }
  function getError(): string | null {
    return storeError;
  }
  function getSelectedJobId(): number | undefined {
    return selectedJobId;
  }
  function getSelectedReviewRevision(): number {
    return selectedReviewRevision;
  }
  function getHighlightedJobId(): number | undefined {
    return highlightedJobId;
  }
  function getFilterRepo(): string | undefined {
    return filterRepo;
  }
  function getFilterBranch(): string | undefined {
    return filterBranch;
  }
  function getFilterStatus(): string | undefined {
    return filterStatus;
  }
  function getFilterSearch(): string | undefined {
    return filterSearch;
  }
  function getFilterHideClosed(): boolean {
    return booleanFilterPreferences.hideClosed;
  }
  function getFilterJobType(): string | undefined {
    return filterJobType;
  }
  function getFilterShowAutoDesign(): boolean {
    return booleanFilterPreferences.showAutoDesign;
  }
  function getSortColumn(): SortColumn {
    return sortColumn;
  }
  function getSortDirection(): SortDirection {
    return sortDirection;
  }
  function canSortJobs(): boolean {
    return allResultsLoaded && !loading;
  }
  function isEventStreamConnected(): boolean {
    return eventStreamConnected;
  }

  function dispose(): void {
    if (disposed) return;
    disposed = true;
    booleanFilterSubscribers.delete(filterOwner);
  }

  booleanFilterSubscribers.set(filterOwner, () => {
    if (!disposed) {
      loadedLimit = DEFAULT_PAGE_LIMIT;
      loadJobs();
    }
  });

  return {
    getJobs,
    getVisibleJobs,
    isLoading,
    getHasMore,
    getStats,
    getFilteredStatusCounts,
    usesFilteredStatusCounts,
    getError,
    getSelectedJobId,
    getSelectedReviewRevision,
    getHighlightedJobId,
    getFilterRepo,
    getFilterBranch,
    getFilterStatus,
    getFilterSearch,
    getFilterHideClosed,
    getFilterJobType,
    getFilterShowAutoDesign,
    getSortColumn,
    getSortDirection,
    canSortJobs,
    isEventStreamConnected,
    dispose,
    togglePanel,
    ensurePanelMembers,
    setPanelMemberInterest,
    refreshPanelMembers,
    isPanelExpanded,
    getPanelMembers,
    getPanelMemberError,
    isLoadingMembers,
    loadJobs,
    loadJobsEffect,
    loadMore,
    loadMoreEffect,
    setFilter,
    setRepoBranchFilter,
    setSortColumn,
    cancelJob,
    cancelJobEffect,
    rerunJob,
    rerunJobEffect,
    isRerunning,
    setSelectedJobId,
    selectJob,
    deselectJob,
    selectNextJob,
    selectPrevJob,
    highlightJob,
    highlightNextJob,
    highlightPrevJob,
    connectEventStream,
    connectEventStreamEffect,
    disconnectEventStream,
    disconnectEventStreamEffect,
  };
}

export type JobsStore = ReturnType<typeof createJobsStore>;
