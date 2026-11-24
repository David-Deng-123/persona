import type { Session } from "./types";

export interface SpeedOption {
  id: string;
  name?: string;
  description?: string;
}

export const STANDARD_FAST: readonly SpeedOption[] = [{ id: "standard" }, { id: "fast" }];

export function speedId(tier: string): string {
  if (tier === "default") return "standard";
  if (tier === "priority") return "fast";
  return tier;
}

/** Keep native tier IDs in the RPC. Older hubs can advertise Fast alone, with Standard implicit. */
export function speedOptions(s: Session | undefined): SpeedOption[] {
  const choices = new Map<string, SpeedOption>();
  if (!s?.service_tiers?.length) return [];
  choices.set("standard", { id: "standard" });
  for (const tier of s.service_tiers) {
    if (!tier.id) continue;
    const id = speedId(tier.id);
    choices.set(id, { ...tier, id });
  }
  if (choices.size < 2) return [];
  const order = ["standard", "fast", "ultrafast"];
  const rank = (id: string) => order.includes(id) ? order.indexOf(id) : order.length;
  return [...choices.values()].sort((a, b) => rank(a.id) - rank(b.id));
}

export function speedTier(s: Session, speed: string): string | undefined {
  if (!speedOptions(s).some((choice) => choice.id === speed)) return undefined;
  return s.service_tiers?.find((tier) => speedId(tier.id) === speed)?.id ?? "default";
}
