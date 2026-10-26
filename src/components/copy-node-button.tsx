import { apiFetch } from "@/lib/api";
import { useState } from "react";
import { Check, Copy, Loader2 } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { copyText } from "@/lib/clipboard";
import { plainName, type NodeRecord } from "@/lib/data";

export function CopyNodeButton({
  node,
  showLabel = false,
}: {
  node: NodeRecord;
  showLabel?: boolean;
}) {
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);
  async function copy(source: HTMLElement) {
    setBusy(true);
    setCopied(false);
    try {
      const response = await apiFetch("/api/nodes/link", {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-Node-Explorer": "1" },
        body: JSON.stringify({ identity: node.identity }),
      });
      const data = await response.json();
      if (!response.ok) throw new Error(data.error || "无法获取节点链接。");
      await copyText(data.content, source);
      setCopied(true);
      toast.success("节点链接已复制");
    } catch (cause) {
      toast.error(
        cause instanceof Error
          ? cause.message
          : "复制失败，请检查剪贴板权限后重试。",
      );
    } finally {
      setBusy(false);
    }
  }
  return (
    <Button
      variant="outline"
      size={showLabel ? "sm" : "icon"}
      disabled={busy || !node.identity}
      title={
        node.identity
          ? "复制数据库保存的原始节点链接"
          : "此节点暂无可复制的连接信息"
      }
      aria-label={`复制 ${plainName(node)} 节点链接`}
      onClick={(event) => {
        event.stopPropagation();
        void copy(event.currentTarget);
      }}
    >
      {busy ? (
        <Loader2 className="size-4 animate-spin" />
      ) : copied ? (
        <Check className="size-4" />
      ) : (
        <Copy className="size-4" />
      )}
      {showLabel && (busy ? "正在复制…" : copied ? "已复制" : "复制节点链接")}
    </Button>
  );
}
