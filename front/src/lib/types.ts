/** Shared type names, aliased to the generated contract (api.d.ts) so they cannot drift. */

import type { components } from "./api";

type S = components["schemas"];

export type HealthStatus = S["HealthStatus"];
export type Monitor = S["Monitor"];
export type Source = S["Source"];
export type AlertChannel = S["AlertChannel"];
export type TimelineEventKind = S["TimelineEventKind"];
export type TimelineEntry = S["TimelineEntry"];
export type Incident = S["Incident"];
