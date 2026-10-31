// Example Strata policies. Copy into the "strata-policies" repository and push:
// they take effect as soon as main moves. Invalid policies are rejected on push.
//
// Entities: User::"<email>" in Group::"<cognito group>", Repo::"<name>" with
// attributes kind ("code" | "layer") and name; a repo is `in` every Layer it
// inherits from. Actions: read, write, annotate, admin, create, approve, and
// audit (on Repo::"_breakglass": see break-glass grants).
//
// Group "strata-admins" has no standing access once these policies exist. An
// admin activates a time-boxed break-glass grant (`strata breakglass start`),
// which overrides any deny here, including forbids, and is reviewed afterwards
// by another admin. Grant the admins' everyday access below like anyone's.

// Everyone signed in can read repositories and link work items to changes.
permit (principal, action in [Action::"read", Action::"annotate"], resource);

// Anyone can create code repositories; only the platform team creates Layers.
permit (principal, action == Action::"create", resource)
when { resource.kind == "code" };
permit (principal in Group::"platform", action == Action::"create", resource)
when { resource.kind == "layer" };

// The platform team owns every Layer and the policies themselves.
permit (principal in Group::"platform", action in [Action::"write", Action::"admin"], resource)
when { resource.kind == "layer" };
permit (principal in Group::"platform", action in [Action::"write", Action::"admin"], resource == Repo::"strata-policies");

// Payments developers write every code repository under the payments Layer.
permit (principal in Group::"payments-devs", action in [Action::"write", Action::"admin"], resource in Repo::"payments-rules")
when { resource.kind == "code" };

// Auditors see break-glass grants and what was done under them.
permit (principal in Group::"auditors", action == Action::"audit", resource == Repo::"_breakglass");

// Key material: only the crypto team, and nobody else may even see it.
permit (principal in Group::"crypto", action, resource == Repo::"hsm-keys");
forbid (principal, action, resource == Repo::"hsm-keys")
unless { principal in Group::"crypto" };
