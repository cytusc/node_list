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
  services: Service[];
  liveness: {
    xrayQueued: number | null;
    mihomoQueued: number | null;
    inFlight: number | null;
    lastCompletedAt: string | null;
    results?: Record<
      "reachable" | "unreachable" | "config_error" | "infrastructure",
      number
    >;
    visibility?: Record<
      "published" | "unavailable" | "expired" | "unverified" | "unverifiedExit",
      number
    >;
  };
};
