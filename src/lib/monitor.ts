export type Queue = {
  kind: string;
  due: number;
  scheduled: number;
  running: number;
  expired: number;
  oldest_seconds: number;
};
export type Coverage = {
  total: number;
  connectivity: number;
  exit: number;
  attributes: number;
};
export type Service = {
  id: string;
  name: string;
  description: string;
  status: "healthy" | "degraded" | "stale" | "unknown";
  updatedAt: string | null;
  message: string;
};
export type MonitorData = {
  generatedAt: string;
  sampledAt: string | null;
  sampleStale: boolean;
  services: Service[];
  current: {
    queues: Queue[];
    coverage: Coverage;
  } | null;
  kernel: {
    healthy: boolean;
    loaded: number | null;
    quarantined: number | null;
    reloadMillis: number | null;
    reloadDrainMillis: number | null;
    reloadPauseMillis: number | null;
    exitCapacity: number | null;
  };
  liveness: {
    xrayQueued: number | null;
    mihomoQueued: number | null;
    inFlight: number | null;
    lastCompletedAt: string | null;
  };
  hours: number;
  hourly: {
    hour: string;
    samples: number;
    queues: Record<string, Pick<Queue, "due" | "scheduled" | "running">> | null;
    coverage: Coverage | null;
  }[];
};
export const queueGroups: Record<string, { label: string; kinds: string[] }> = {
  all: { label: "所有任务", kinds: [] },
  "exit-provision": {
    label: "出口检测",
    kinds: ["exit"],
  },
  "exit-attributes": { label: "IP 属性", kinds: ["attributes", "risk"] },
};
export function totals(queues: Queue[], group = "all") {
  const kinds = queueGroups[group]?.kinds ?? [];
  return queues
    .filter((q) => !kinds.length || kinds.includes(q.kind))
    .reduce(
      (a, q) => ({
        due: a.due + q.due,
        scheduled: a.scheduled + q.scheduled,
        running: a.running + q.running,
        expired: a.expired + q.expired,
        oldest_seconds: Math.max(a.oldest_seconds, q.oldest_seconds),
      }),
      { due: 0, scheduled: 0, running: 0, expired: 0, oldest_seconds: 0 },
    );
}
export function percent(value: number, total: number) {
  return total > 0 ? Math.max(0, Math.min(100, (value / total) * 100)) : 0;
}
