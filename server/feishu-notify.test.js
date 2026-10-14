import { test } from "node:test";
import assert from "node:assert/strict";
import { createFeishuNotifier } from "./feishu-notify.js";

const okResponse = () => ({
  ok: true,
  status: 200,
  json: async () => ({ code: 0, msg: "success" }),
});

test("Feishu notifier posts a text message to the webhook", async () => {
  const calls = [];
  const notify = createFeishuNotifier({
    webhook: "https://open.feishu.cn/open-apis/bot/v2/hook/test",
    fetchImpl: async (url, options) => {
      calls.push([url, JSON.parse(options.body)]);
      return okResponse();
    },
  });
  const ok = await notify("⚠️ Qilan 登录状态失效");
  assert.equal(ok, true);
  assert.equal(calls.length, 1);
  assert.equal(calls[0][0], "https://open.feishu.cn/open-apis/bot/v2/hook/test");
  assert.deepEqual(calls[0][1], {
    msg_type: "text",
    content: { text: "⚠️ Qilan 登录状态失效" },
  });
});

test("Feishu notifier accepts the legacy StatusCode response shape", async () => {
  const notify = createFeishuNotifier({
    webhook: "https://open.feishu.cn/hook",
    fetchImpl: async () => ({
      ok: true,
      status: 200,
      json: async () => ({ StatusCode: 0, StatusMessage: "success" }),
    }),
  });
  assert.equal(await notify("hello"), true);
});

test("Feishu notifier throttles identical messages within the window", async () => {
  let clock = 1000;
  const calls = [];
  const notify = createFeishuNotifier({
    webhook: "https://open.feishu.cn/hook",
    throttleMs: 30 * 60_000,
    now: () => clock,
    fetchImpl: async () => {
      calls.push(1);
      return okResponse();
    },
  });
  assert.equal(await notify("same"), true);
  clock += 60_000;
  assert.equal(await notify("same"), false); // throttled, no HTTP call
  assert.equal(await notify("different"), true); // different text bypasses
  clock += 31 * 60_000;
  assert.equal(await notify("same"), true); // window elapsed
  assert.equal(calls.length, 3);
});

test("Feishu notifier does not record a send when the webhook rejects", async () => {
  let clock = 1000;
  let reject = true;
  const notify = createFeishuNotifier({
    webhook: "https://open.feishu.cn/hook",
    now: () => clock,
    fetchImpl: async () =>
      reject
        ? { ok: false, status: 400, json: async () => ({ code: 19021, msg: "sign error" }) }
        : okResponse(),
  });
  assert.equal(await notify("alert"), false);
  reject = false;
  assert.equal(await notify("alert"), true); // retried after failure
  clock += 1000;
  assert.equal(await notify("alert"), false); // now throttled
});

test("Feishu notifier without a webhook is a no-op", async () => {
  const notify = createFeishuNotifier({});
  assert.equal(await notify("ignored"), false);
});
