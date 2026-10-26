import { useEffect, useState, type ReactNode, type FormEvent } from "react";
import { Loader2, LockKeyhole, LogOut, Network } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";

export function AuthGate({ children }: { children: ReactNode }) {
  const [authenticated, setAuthenticated] = useState<boolean | null>(null);
  const [configured, setConfigured] = useState(true);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [checking, setChecking] = useState(false);

  async function checkSession(signal?: AbortSignal) {
    setChecking(true);
    setError("");
    try {
      const response = await fetch("/api/auth/session", {
        cache: "no-store",
        signal,
      });
      if (!response.ok) throw new Error("无法检查登录状态，请重试。");
      const data = await response.json();
      if (signal?.aborted) return;
      setAuthenticated(data.authenticated === true);
      setConfigured(data.configured !== false);
    } catch {
      if (!signal?.aborted) setError("无法连接登录服务，请检查网络后重试。");
    } finally {
      if (!signal?.aborted) setChecking(false);
    }
  }

  useEffect(() => {
    const controller = new AbortController();
    void checkSession(controller.signal);
    function onExpired() {
      setAuthenticated(false);
      setError("登录已过期，请重新登录。");
    }
    window.addEventListener("node-explorer:unauthenticated", onExpired);
    return () => {
      controller.abort();
      window.removeEventListener("node-explorer:unauthenticated", onExpired);
    };
  }, []);

  async function login(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    setBusy(true);
    setError("");
    try {
      const response = await fetch("/api/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-Node-Explorer": "1" },
        body: JSON.stringify({
          username: form.get("username"),
          password: form.get("password"),
        }),
      });
      const data = await response.json();
      if (!response.ok) throw new Error(data.error || "登录失败，请稍后重试。");
      setAuthenticated(true);
      if (window.location.pathname === "/login")
        window.history.replaceState(null, "", "/");
    } catch (cause) {
      setError(
        cause instanceof Error ? cause.message : "网络异常，请稍后重试。",
      );
    } finally {
      setBusy(false);
    }
  }

  async function logout() {
    setBusy(true);
    setError("");
    try {
      const response = await fetch("/api/auth/logout", {
        method: "POST",
        headers: { "X-Node-Explorer": "1" },
      });
      if (!response.ok) throw new Error("退出失败，请重试。");
      setAuthenticated(false);
    } catch {
      setError("退出失败，请检查网络后重试。");
    } finally {
      setBusy(false);
    }
  }

  if (authenticated)
    return (
      <>
        <div className="flex items-center justify-end gap-3 border-b bg-background px-4 py-1.5 text-sm">
          {error && (
            <span role="alert" className="text-destructive">
              {error}
            </span>
          )}
          <Button
            variant="ghost"
            size="sm"
            disabled={busy}
            onClick={() => void logout()}
          >
            <LogOut className="size-4" />
            退出登录
          </Button>
        </div>
        {children}
      </>
    );

  return (
    <main className="grid min-h-svh place-items-center bg-muted/35 p-4">
      <div className="w-full max-w-sm space-y-6">
        <div className="flex items-center justify-center gap-2 font-semibold">
          <Network className="size-5 text-primary" />
          Node Explorer
        </div>
        <Card>
          <CardHeader>
            <div className="mb-3 flex size-11 items-center justify-center rounded-xl bg-primary/10 text-primary">
              <LockKeyhole className="size-5" />
            </div>
            <CardTitle className="text-xl">登录节点面板</CardTitle>
            <CardDescription>
              登录后查看节点、复制连接信息和访问服务监控。
            </CardDescription>
          </CardHeader>
          <CardContent>
            {authenticated === null ? (
              <div className="space-y-4" role="status">
                {checking && (
                  <p className="flex items-center gap-2 text-sm text-muted-foreground">
                    <Loader2 className="size-4 animate-spin" />
                    正在检查登录状态…
                  </p>
                )}
                {error && (
                  <>
                    <p role="alert" className="text-sm text-destructive">
                      {error}
                    </p>
                    <Button
                      variant="outline"
                      onClick={() => void checkSession()}
                      disabled={checking}
                    >
                      重试
                    </Button>
                  </>
                )}
              </div>
            ) : (
              <form onSubmit={login} className="space-y-5">
                <div className="space-y-2">
                  <label htmlFor="username" className="text-sm font-medium">
                    账号
                  </label>
                  <Input
                    id="username"
                    name="username"
                    autoComplete="username"
                    required
                    autoFocus
                    maxLength={256}
                    disabled={busy || !configured}
                  />
                </div>
                <div className="space-y-2">
                  <label htmlFor="password" className="text-sm font-medium">
                    密码
                  </label>
                  <Input
                    id="password"
                    name="password"
                    type="password"
                    autoComplete="current-password"
                    required
                    maxLength={1024}
                    disabled={busy || !configured}
                  />
                </div>
                {!configured && (
                  <p role="alert" className="text-sm text-destructive">
                    管理员尚未配置登录账号，请联系管理员完成配置。
                  </p>
                )}
                {error && (
                  <p role="alert" className="text-sm text-destructive">
                    {error}
                  </p>
                )}
                <Button
                  type="submit"
                  className="w-full"
                  disabled={busy || !configured}
                >
                  {busy && <Loader2 className="size-4 animate-spin" />}
                  {busy ? "正在登录…" : "登录"}
                </Button>
              </form>
            )}
          </CardContent>
        </Card>
        <p className="text-center text-xs text-muted-foreground">
          仅限授权访问 · 请妥善保管账号与节点链接
        </p>
      </div>
    </main>
  );
}
