// Four-eyes on main: a ref move needs one approval from someone who did not
// write any change in the range, a linked work item, and must be a
// fast-forward (no rollback, no delete). `forbid` also binds strata-admins;
// only a time-boxed break-glass grant (strata breakglass start) overrides it.
//
// The server computes the context for every write that moves a ref:
//   context.ref          name of the ref being moved ("" if none)
//   context.fastForward  the new target descends from the current one
//   context.approvals    distinct approvers of the target, authors excluded
//   context.hasWorkItem  the change (plus annotations) links a work item
//
// Flow: strata push --review NAME -m "PAY-42 ..."  ->  colleague runs
// strata approve REV  ->  strata promote REV --ref main.
//
// Beware: if this protects the policy repository too, changing policies needs
// a second person as well. With a single admin, add a second user first.
forbid (principal, action == Action::"write", resource)
when {
  context.ref == "main" &&
  (context.approvals < 1 || !context.fastForward || !context.hasWorkItem)
};
