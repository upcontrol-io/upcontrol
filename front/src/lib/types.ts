/**
 * Shared type names, aliased to the generated OpenAPI contract (api.d.ts) so
 * they can never drift from it.
 */

import type { components } from "./api";

type S = components["schemas"];

export type Plan = S["Plan"];
export type HealthStatus = S["HealthStatus"];
export type Billing = S["Billing"];
export type Account = S["Account"];
export type Project = S["Project"];
export type Monitor = S["Monitor"];
export type Source = S["Source"];
export type ConnectableSource = S["ConnectableSource"];
export type PublicComponent = S["PublicComponent"];
export type ChannelKind = S["ChannelKind"];
export type NotifySettings = S["NotifySettings"];
export type AlertChannel = S["AlertChannel"];
export type RecipientRole = S["RecipientRole"];
export type Recipient = S["Recipient"];
export type TimelineEventKind = S["TimelineEventKind"];
export type TimelineEntry = S["TimelineEntry"];
export type IncidentResult = S["IncidentResult"];
export type Incident = S["Incident"];
export type LogLevel = S["LogLevel"];
export type WatchStatus = S["WatchStatus"];
export type WatchRow = S["WatchRow"];
export type WatchGroup = S["WatchGroup"];
export type NetworkCheck = S["NetworkCheck"];
export type ApiKey = S["ApiKey"];
export type KeyUsageEntry = S["KeyUsageEntry"];
export type Metric = S["Metric"];
