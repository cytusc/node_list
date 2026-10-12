export const EXIT_IP_VERSION = 3;

export function hasVerifiedExitIP(result) {
  return (
    result?.exitIPSource === "ip.sb" ||
    (result?.exitIPMode === "proxy-echo" &&
      result?.exitIPVersion === EXIT_IP_VERSION)
  );
}
