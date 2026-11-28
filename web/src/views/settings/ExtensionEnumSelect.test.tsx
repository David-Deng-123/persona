// @vitest-environment jsdom
// Component and integration tests for extension configuration enum controls (<= 8 native <select> vs > 8 SearchableSelect)
// across both classic Extensions settings and unified IntegrationDetail.
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { connectModuleHub } from "../../core/module-host";
import { t } from "../../i18n";
import { ApiError, setToken } from "../../lib/api";
import type { ExtensionChoice, HubExtensionDetail, JsonObject } from "../../lib/extensions";
import { resetManagerChoice } from "../../lib/manager-choice";
import { setRoster } from "../../lib/managers";
import { applyCompleteSessions, getState, subscribe } from "../../lib/store";

const hub = vi.hoisted(() => ({
  detail: null as HubExtensionDetail | null,
  savedConfigs: [] as JsonObject[],
  saveError: null as Error | null,
}));

vi.mock("../../lib/extensions", async (original) => ({
  ...(await original<typeof import("../../lib/extensions")>()),
  extensionDetail: vi.fn(() => Promise.resolve(structuredClone(hub.detail!))),
  setExtensionConfig: vi.fn((_id: string, config: JsonObject) => {
    if (hub.saveError) return Promise.reject(hub.saveError);
    hub.savedConfigs.push(structuredClone(config));
    hub.detail = { ...hub.detail!, config: structuredClone(config) };
    return Promise.resolve(structuredClone(hub.detail));
  }),
}));
vi.mock("../../ui/atoms", async (original) => ({
  ...(await original<typeof import("../../ui/atoms")>()),
  toast: vi.fn(),
}));

import { setExtensionConfig } from "../../lib/extensions";
import { Extensions } from "./Extensions";
import { IntegrationDetail } from "./IntegrationDetail";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const SHORT_CHOICES: ExtensionChoice[] = Array.from({ length: 8 }, (_, i) => ({
  value: `opt_${i + 1}`,
  title: `Option ${i + 1}`,
}));

const LONG_CHOICES: ExtensionChoice[] = [
  { value: "US", title: "美国 (US · United States)" },
  { value: "CN", title: "中国大陆 (CN · China)" },
  { value: "HK", title: "中国香港 (HK · Hong Kong)" },
  { value: "TW", title: "中国台湾 (TW · Taiwan)" },
  { value: "JP", title: "日本 (JP · Japan)" },
  { value: "KR", title: "韩国 (KR · South Korea)" },
  { value: "SG", title: "新加坡 (SG · Singapore)" },
  { value: "DE", title: "德国 (DE · Germany)" },
  { value: "FR", title: "法国 (FR · France)" },
];

const makeDetail = (config: JsonObject = {}): HubExtensionDetail => ({
  id: "store-ext",
  name: "Store Extension",
  kind: "process",
  status: "running",
  source: "installed",
  enabled: true,
  form: {
    fields: [
      { name: "short_mode", type: "string", title: "Short Mode", enum: SHORT_CHOICES },
      { name: "region", type: "string", title: "Store Region", enum: LONG_CHOICES },
    ],
  },
  config,
});

const ROSTER = {
  default: "assistant",
  managers: [{ id: "assistant", name: "Default", builtin: true, online: true }],
};
const SEED_SESSION = { machine_id: "hub", session_id: "seed-1", status: "idle", updated_at: "2026-01-01T00:00:00Z" } as never;

let host: HTMLDivElement;
let root: Root;
let login = 0;

const flush = async () => {
  for (let i = 0; i < 10; i++) {
    await act(async () => {
      await Promise.resolve();
    });
  }
};

function typeInto(input: HTMLInputElement, value: string) {
  Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!.call(input, value);
  input.dispatchEvent(new Event("input", { bubbles: true }));
}

function keyDown(target: HTMLElement, key: string) {
  target.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true }));
}

function findButton(label: string) {
  return [...host.querySelectorAll<HTMLButtonElement>("button")].find((b) => b.textContent?.trim() === label);
}

beforeEach(() => {
  setToken(`enum-token-${++login}`);
  resetManagerChoice();
  setRoster(ROSTER);
  applyCompleteSessions([SEED_SESSION]);
  applyCompleteSessions([SEED_SESSION], [], {
    features: ["extensions"],
    assistant: { machine_id: "hub", session_id: "main-1", manager: "assistant", integrations_v2: true, conversations: [] } as never,
  });
  connectModuleHub(getState, subscribe);

  hub.detail = makeDetail({ region: "US" });
  hub.savedConfigs = [];
  hub.saveError = null;
  vi.mocked(setExtensionConfig).mockClear();

  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});

afterEach(() => {
  act(() => root.unmount());
  host.remove();
});

describe("extension configuration enum controls in classic Extensions", () => {
  it("keeps native <select> for <= 8 options and upgrades > 8 options to SearchableSelect", async () => {
    act(() => root.render(<Extensions open="store-ext" onOpen={() => {}} />));
    await flush();

    const shortEl = host.querySelector<HTMLElement>("#extension-field-short_mode")!;
    expect(shortEl.tagName).toBe("SELECT");

    const regionEl = host.querySelector<HTMLButtonElement>("#extension-field-region")!;
    expect(regionEl.tagName).toBe("BUTTON");
    expect(regionEl.getAttribute("aria-haspopup")).toBe("listbox");
    expect(regionEl.textContent).toContain("美国 (US · United States)");
  });

  it("marks form dirty, saves selected option, supports clearing back to default, and surfaces validation errors", async () => {
    act(() => root.render(<Extensions open="store-ext" onOpen={() => {}} />));
    await flush();

    const trigger = host.querySelector<HTMLButtonElement>("#extension-field-region")!;
    // Select CN via search + Enter
    act(() => trigger.click());
    const search = document.body.querySelector<HTMLInputElement>('input[role="combobox"]')!;
    act(() => typeInto(search, "中国 cn"));
    act(() => keyDown(search, "Enter"));

    expect(trigger.textContent).toContain("中国大陆 (CN · China)");
    const saveBtn = findButton(t("common.save"))!;
    expect(saveBtn).toBeTruthy();

    // Simulate a server invalid_config error on region
    hub.saveError = new ApiError(400, "invalid region", { code: "invalid_config", field: "region", message: "Region not supported" });
    await act(async () => saveBtn.click());
    await flush();
    expect(trigger.getAttribute("aria-invalid")).toBe("true");
    expect(host.textContent).toContain("Region not supported");

    // Pick JP; error clears and Save persists { region: "JP" }
    hub.saveError = null;
    act(() => trigger.click());
    const search2 = document.body.querySelector<HTMLInputElement>('input[role="combobox"]')!;
    act(() => typeInto(search2, "JP"));
    act(() => keyDown(search2, "Enter"));
    expect(trigger.getAttribute("aria-invalid")).toBeNull();

    await act(async () => findButton(t("common.save"))!.click());
    await flush();
    expect(hub.savedConfigs).toEqual([{ region: "JP" }]);

    // Clear back to undefined via default placeholder option and save
    act(() => trigger.click());
    const clearRow = document.body.querySelector<HTMLButtonElement>('[data-clear-option="true"]')!;
    act(() => clearRow.click());
    expect(trigger.textContent).toContain(t("lib-extensions.not_set"));

    await act(async () => findButton(t("common.save"))!.click());
    await flush();
    expect(hub.savedConfigs[1]).toEqual({});
  });

  it("reverts unsaved SearchableSelect changes cleanly", async () => {
    act(() => root.render(<Extensions open="store-ext" onOpen={() => {}} />));
    await flush();

    const trigger = host.querySelector<HTMLButtonElement>("#extension-field-region")!;
    act(() => trigger.click());
    const search = document.body.querySelector<HTMLInputElement>('input[role="combobox"]')!;
    act(() => typeInto(search, "DE"));
    act(() => keyDown(search, "Enter"));
    expect(trigger.textContent).toContain("德国 (DE · Germany)");

    const revertBtn = findButton(t("settings-extensions.revert"))!;
    expect(revertBtn).toBeTruthy();
    act(() => revertBtn.click());
    expect(trigger.textContent).toContain("美国 (US · United States)");
    expect(findButton(t("common.save"))).toBeUndefined();
  });
});

describe("extension configuration enum controls in unified IntegrationDetail", () => {
  it("shares the threshold upgrade, search/select, and dirty/save flow", async () => {
    hub.detail = makeDetail({});
    act(() => root.render(<IntegrationDetail target={{ kind: "extension", id: "store-ext" }} integrationsV2 />));
    await flush();

    // <= 8 stays native <select>; > 8 upgrades to SearchableSelect
    expect(host.querySelector("#extension-field-short_mode")?.tagName).toBe("SELECT");
    const trigger = host.querySelector<HTMLButtonElement>("#extension-field-region")!;
    expect(trigger.tagName).toBe("BUTTON");
    expect(trigger.getAttribute("aria-haspopup")).toBe("listbox");
    expect(trigger.textContent).toContain(t("lib-extensions.not_set"));

    // Search "singapore" and select via Enter
    act(() => trigger.click());
    const search = document.body.querySelector<HTMLInputElement>('input[role="combobox"]')!;
    act(() => typeInto(search, "singapore"));
    act(() => keyDown(search, "Enter"));
    expect(trigger.textContent).toContain("新加坡 (SG · Singapore)");

    // Save through unified IntegrationDetail save bar
    const saveBtn = findButton(t("common.save"))!;
    expect(saveBtn).toBeTruthy();
    await act(async () => saveBtn.click());
    await flush();
    expect(hub.savedConfigs).toEqual([{ region: "SG" }]);
  });
});
