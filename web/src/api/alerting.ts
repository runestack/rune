/* Alerting API — thin wrappers over ObserveService's alert-rule and
   notification-channel RPCs. A rule holds a PLAIN LogQL selector (the alerter
   wraps it in count_over_time) plus a threshold condition; channels are
   templated webhooks. The server validates LogQL, names and durations at save
   time, so surface its error messages — they're written for operators. */

import {
  AlertRule as AlertRuleMsg,
  AlertStatus as AlertStatusMsg,
  Channel as ChannelMsg,
  DeleteAlertRuleRequest,
  DeleteChannelRequest,
  ListAlertRulesRequest,
  ListChannelsRequest,
  SaveAlertRuleRequest,
  SaveChannelRequest,
} from "../gen/pkg/api/proto/observe_pb";
import { clients } from "./transport";

export interface AlertRuleData {
  id: string;
  name: string; // dns-1123, cluster-unique
  description: string;
  logql: string; // plain log selector — no count_over_time wrapper
  window: string; // rolling count window, Go duration ("5m")
  op: string; // > >= < <= ==
  threshold: number;
  for: string; // pending->firing hysteresis ("" = fire immediately)
  interval: string; // evaluation cadence ("" = 60s)
  channels: string[]; // channel names
  disabled: boolean;
  createdBy: string;
  createdAt: string; // RFC3339
  updatedAt: string; // RFC3339
}

export interface AlertStatusData {
  rule: string; // rule name
  state: "ok" | "pending" | "firing";
  value: number;
  since: string; // RFC3339 — when the current state began
  lastEval: string; // RFC3339
  lastError: string; // "" when healthy
}

export interface ChannelData {
  id: string;
  name: string; // dns-1123, cluster-unique
  type: string; // webhook | slack
  url: string;
  headers: Record<string, string>; // values may hold ${secret:ns/name/key}
  body: string; // Go text/template; "" = default payload
  createdAt: string; // RFC3339
  updatedAt: string; // RFC3339
}

function ruleFromMsg(m: AlertRuleMsg): AlertRuleData {
  return {
    id: m.id,
    name: m.name,
    description: m.description,
    logql: m.logql,
    window: m.window,
    op: m.op,
    threshold: m.threshold,
    for: m.for,
    interval: m.interval,
    channels: m.channels,
    disabled: m.disabled,
    createdBy: m.createdBy,
    createdAt: m.createdAt,
    updatedAt: m.updatedAt,
  };
}

function statusFromMsg(m: AlertStatusMsg): AlertStatusData {
  return {
    rule: m.rule,
    state: (m.state as AlertStatusData["state"]) || "ok",
    value: m.value,
    since: m.since,
    lastEval: m.lastEval,
    lastError: m.lastError,
  };
}

function channelFromMsg(m: ChannelMsg): ChannelData {
  return {
    id: m.id,
    name: m.name,
    type: m.type,
    url: m.url,
    headers: { ...m.headers },
    body: m.body,
    createdAt: m.createdAt,
    updatedAt: m.updatedAt,
  };
}

/** List every alert rule plus the alerter's live status per rule. */
export async function listAlertRules(): Promise<{ rules: AlertRuleData[]; statuses: AlertStatusData[] }> {
  const res = await clients.observe.listAlertRules(new ListAlertRulesRequest());
  return { rules: res.rules.map(ruleFromMsg), statuses: res.statuses.map(statusFromMsg) };
}

/** Upsert a rule by name. Returns the stored rule (with stamped identity). */
export async function saveAlertRule(r: {
  name: string;
  description?: string;
  logql: string;
  window: string;
  op: string;
  threshold: number;
  for?: string;
  interval?: string;
  channels?: string[];
  disabled?: boolean;
}): Promise<AlertRuleData> {
  const res = await clients.observe.saveAlertRule(
    new SaveAlertRuleRequest({
      rule: new AlertRuleMsg({
        name: r.name,
        description: r.description ?? "",
        logql: r.logql,
        window: r.window,
        op: r.op,
        threshold: r.threshold,
        for: r.for ?? "",
        interval: r.interval ?? "",
        channels: r.channels ?? [],
        disabled: r.disabled ?? false,
      }),
    }),
  );
  if (!res.rule) throw new Error("saveAlertRule: empty response");
  return ruleFromMsg(res.rule);
}

/** Delete a rule by name. */
export async function deleteAlertRule(name: string): Promise<void> {
  await clients.observe.deleteAlertRule(new DeleteAlertRuleRequest({ name }));
}

/** List every notification channel. */
export async function listChannels(): Promise<ChannelData[]> {
  const res = await clients.observe.listChannels(new ListChannelsRequest());
  return res.channels.map(channelFromMsg);
}

/** Upsert a channel by name. Returns the stored channel. */
export async function saveChannel(c: {
  name: string;
  type: string;
  url: string;
  headers?: Record<string, string>;
  body?: string;
}): Promise<ChannelData> {
  const res = await clients.observe.saveChannel(
    new SaveChannelRequest({
      channel: new ChannelMsg({
        name: c.name,
        type: c.type,
        url: c.url,
        headers: c.headers ?? {},
        body: c.body ?? "",
      }),
    }),
  );
  if (!res.channel) throw new Error("saveChannel: empty response");
  return channelFromMsg(res.channel);
}

/** Delete a channel by name. */
export async function deleteChannel(name: string): Promise<void> {
  await clients.observe.deleteChannel(new DeleteChannelRequest({ name }));
}
