// @vitest-environment jsdom
// Unit tests for SearchableSelect matching/ranking helpers and interactive component behavior.
import { act, useState } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { t } from "../i18n";
import {
  filterSearchableOptions,
  optionSecondaryHint,
  rankSearchableOption,
  SearchableSelect,
  SearchMatchRank,
  type SearchableSelectOption,
} from "./SearchableSelect";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const REGIONS: SearchableSelectOption<string>[] = [
  { value: "US", label: "美国 (US · United States)" },
  { value: "CN_HK", label: "中国香港 (CN_HK · Hong Kong)" },
  { value: "CN", label: "中国大陆 (CN · China)" },
  { value: "JP", label: "日本 (JP · Japan)" },
  { value: "UK", label: "United Kingdom" },
  { value: "AU", label: "Australia" },
  { value: "DE", label: "德国 (DE · Germany)" },
  { value: "FR", label: "法国 (FR · France)" },
  { value: "KR", label: "韩国 (KR · South Korea)" },
  { value: "SG", label: "新加坡 (SG · Singapore)" },
];

describe("SearchableSelect search and ranking helpers", () => {
  it("returns all options in declaration order when query is empty or whitespace", () => {
    expect(filterSearchableOptions(REGIONS, "")).toEqual(REGIONS);
    expect(filterSearchableOptions(REGIONS, "   ")).toEqual(REGIONS);
  });

  it("ranks exact value > exact label > prefix > substring, preserving declaration order on ties", () => {
    const options: SearchableSelectOption<string>[] = [
      { value: "SUB_ALPHA", label: "Contains jp in middle" },
      { value: "JP_EAST", label: "Japan East" },
      { value: "OTHER", label: "jp" },
      { value: "JP", label: "日本 (JP · Japan)" },
      { value: "JP_WEST", label: "Japan West" },
    ];

    const ranked = filterSearchableOptions(options, "jp");
    expect(ranked.map((o) => o.value)).toEqual([
      "JP",        // exact value match (rank 0)
      "OTHER",     // exact label match (rank 1)
      "JP_EAST",   // value prefix match (rank 2, earlier in declaration order)
      "JP_WEST",   // value prefix match (rank 2, later in declaration order)
      "SUB_ALPHA", // substring match (rank 3)
    ]);

    expect(rankSearchableOption(options[3], "JP")).toBe(SearchMatchRank.ExactValue);
    expect(rankSearchableOption(options[2], "JP")).toBe(SearchMatchRank.ExactLabel);
    expect(rankSearchableOption(options[1], "JP")).toBe(SearchMatchRank.Prefix);
    expect(rankSearchableOption(options[0], "JP")).toBe(SearchMatchRank.Substring);
  });

  it("matches case-insensitively across bilingual Chinese, English labels, and raw value codes", () => {
    expect(filterSearchableOptions(REGIONS, "cn").map((o) => o.value)).toEqual(["CN", "CN_HK"]);
    expect(filterSearchableOptions(REGIONS, "CHINA").map((o) => o.value)).toEqual(["CN"]);
    expect(filterSearchableOptions(REGIONS, "中国").map((o) => o.value)).toEqual(["CN_HK", "CN"]);
    expect(filterSearchableOptions(REGIONS, "日本").map((o) => o.value)).toEqual(["JP"]);
  });

  it("supports multi-token whitespace queries with AND semantics across label and raw value", () => {
    expect(filterSearchableOptions(REGIONS, "cn china").map((o) => o.value)).toEqual(["CN"]);
    expect(filterSearchableOptions(REGIONS, "中国 cn").map((o) => o.value)).toEqual(["CN_HK", "CN"]);
    expect(filterSearchableOptions(REGIONS, "中国 大陆").map((o) => o.value)).toEqual(["CN"]);
    // Token across separate label + rawValue fields when rawValue is not inside label
    const split: SearchableSelectOption<string>[] = [
      { value: "UK", label: "United Kingdom" },
      { value: "US", label: "United States" },
    ];
    expect(filterSearchableOptions(split, "united uk").map((o) => o.value)).toEqual(["UK"]);
    expect(filterSearchableOptions(split, "united jp")).toEqual([]);
  });

  it("preserves declaration order as tie-breaker within the same tier", () => {
    const options: SearchableSelectOption<string>[] = [
      { value: "eu-west-1", label: "Europe (Ireland)" },
      { value: "eu-central-1", label: "Europe (Frankfurt)" },
      { value: "eu-north-1", label: "Europe (Stockholm)" },
    ];
    expect(filterSearchableOptions(options, "eu").map((o) => o.value)).toEqual([
      "eu-west-1",
      "eu-central-1",
      "eu-north-1",
    ]);
  });

  it("shows secondary code hints only when raw value is distinct and not already shown as a token in the label", () => {
    expect(optionSecondaryHint("中国大陆 (CN · China)", "CN")).toBeNull();
    expect(optionSecondaryHint("CN", "CN")).toBeNull();
    expect(optionSecondaryHint("United Kingdom", "UK")).toBe("UK");
    // Ensure short code "us" does not falsely suppress hint inside "Australia"
    expect(optionSecondaryHint("Australia", "us")).toBe("us");
  });
});

let host: HTMLDivElement;
let root: Root;

function typeInto(input: HTMLInputElement, value: string) {
  Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!.call(input, value);
  input.dispatchEvent(new Event("input", { bubbles: true }));
}

function keyDown(target: HTMLElement, key: string) {
  target.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true }));
}

function Harness({
  initial,
  invalid = false,
  onChangeSpy,
}: {
  initial?: string;
  invalid?: boolean;
  onChangeSpy?: (next: string | undefined) => void;
}) {
  const [value, setValue] = useState<string | undefined>(initial);
  return (
    <div>
      <label htmlFor="region-select">Store Region</label>
      <SearchableSelect
        id="region-select"
        title="Store Region"
        value={value}
        options={REGIONS}
        placeholder="Auto-follow account"
        invalid={invalid}
        onChange={(next) => {
          setValue(next);
          onChangeSpy?.(next);
        }}
      />
      <button type="button" data-outside>
        Outside
      </button>
    </div>
  );
}

beforeEach(() => {
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});

afterEach(() => {
  act(() => root.unmount());
  host.remove();
});

describe("SearchableSelect component", () => {
  it("renders accessible trigger attributes, opens popover, selects an option, and clears back to undefined", () => {
    const spy = vi.fn();
    act(() => root.render(<Harness initial="US" invalid onChangeSpy={spy} />));

    const trigger = host.querySelector<HTMLButtonElement>("#region-select")!;
    expect(trigger).toBeTruthy();
    expect(trigger.getAttribute("aria-haspopup")).toBe("listbox");
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(trigger.getAttribute("aria-invalid")).toBe("true");
    expect(trigger.textContent).toContain("美国 (US · United States)");

    // Open picker
    act(() => trigger.click());
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    const listbox = document.body.querySelector<HTMLElement>('[role="listbox"]')!;
    expect(listbox).toBeTruthy();

    // Auto-focused search input on desktop
    const search = document.body.querySelector<HTMLInputElement>('input[role="combobox"]')!;
    expect(document.activeElement).toBe(search);

    // Filter to CN
    act(() => typeInto(search, "cn china"));
    const matching = [...listbox.querySelectorAll<HTMLButtonElement>('[role="option"]')];
    expect(matching).toHaveLength(1);
    expect(matching[0].textContent).toContain("中国大陆 (CN · China)");

    // Select CN
    act(() => matching[0].click());
    expect(spy).toHaveBeenLastCalledWith("CN");
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(trigger.textContent).toContain("中国大陆 (CN · China)");

    // Reopen and clear back to default/placeholder (undefined)
    act(() => trigger.click());
    const clearBtn = document.body.querySelector<HTMLButtonElement>('[data-clear-option="true"]')!;
    expect(clearBtn.textContent).toContain("Auto-follow account");
    act(() => clearBtn.click());
    expect(spy).toHaveBeenLastCalledWith(undefined);
    expect(trigger.textContent).toContain("Auto-follow account");
  });

  it("shows localized empty state when no options match and supports one-click clear-query button", () => {
    act(() => root.render(<Harness />));

    const trigger = host.querySelector<HTMLButtonElement>("#region-select")!;
    act(() => trigger.click());
    const search = document.body.querySelector<HTMLInputElement>('input[role="combobox"]')!;

    act(() => typeInto(search, "no-such-region-xyz"));
    expect(document.body.querySelectorAll('[role="option"]')).toHaveLength(0);
    expect(document.body.querySelector('[role="status"]')?.textContent).toBe(t("ui-searchableselect.no_match"));

    // Click the clear button inside SearchField
    const clearQuery = document.body.querySelector<HTMLButtonElement>(`button[aria-label="${t("ui-searchfield.clear")}"]`)!;
    expect(clearQuery).toBeTruthy();
    act(() => clearQuery.click());
    expect(search.value).toBe("");
    // Default clear option + 10 regions are visible again
    expect(document.body.querySelectorAll('[role="option"]')).toHaveLength(REGIONS.length + 1);
  });

  it("supports ArrowDown, ArrowUp, Enter selection, and Escape dismissal restoring focus without mutating value", () => {
    const spy = vi.fn();
    act(() => root.render(<Harness initial="US" onChangeSpy={spy} />));

    const trigger = host.querySelector<HTMLButtonElement>("#region-select")!;
    // ArrowDown on trigger opens the picker
    act(() => keyDown(trigger, "ArrowDown"));
    expect(trigger.getAttribute("aria-expanded")).toBe("true");

    const search = document.body.querySelector<HTMLInputElement>('input[role="combobox"]')!;
    // Initially active on currently selected option ("US" at index 1, after clear row at index 0)
    const options = () => [...document.body.querySelectorAll<HTMLButtonElement>('[role="option"]')];
    expect(options()[1].getAttribute("data-active")).toBe("true");

    // ArrowDown moves to index 2 ("CN_HK"), ArrowDown again to index 3 ("CN"), ArrowUp back to index 2
    act(() => keyDown(search, "ArrowDown"));
    expect(options()[2].getAttribute("data-active")).toBe("true");
    act(() => keyDown(search, "ArrowDown"));
    expect(options()[3].getAttribute("data-active")).toBe("true");
    act(() => keyDown(search, "ArrowUp"));
    expect(options()[2].getAttribute("data-active")).toBe("true");

    // Escape closes without changing value and restores focus to trigger
    act(() => keyDown(search, "Escape"));
    expect(spy).not.toHaveBeenCalled();
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(document.activeElement).toBe(trigger);

    // Reopen, filter to "jp", press Enter to select "JP", closing and restoring focus to trigger
    act(() => trigger.click());
    const search2 = document.body.querySelector<HTMLInputElement>('input[role="combobox"]')!;
    act(() => typeInto(search2, "jp"));
    act(() => keyDown(search2, "Enter"));
    expect(spy).toHaveBeenCalledWith("JP");
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(document.activeElement).toBe(trigger);
  });

  it("dismisses on outside pointerdown without mutating value", () => {
    const spy = vi.fn();
    act(() => root.render(<Harness initial="JP" onChangeSpy={spy} />));

    const trigger = host.querySelector<HTMLButtonElement>("#region-select")!;
    act(() => trigger.click());
    expect(trigger.getAttribute("aria-expanded")).toBe("true");

    const outside = host.querySelector<HTMLButtonElement>("[data-outside]")!;
    act(() => {
      outside.dispatchEvent(new Event("pointerdown", { bubbles: true }));
    });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(spy).not.toHaveBeenCalled();
  });

  it("closes without mutating when Escape is pressed with a non-empty search query", () => {
    const spy = vi.fn();
    act(() => root.render(<Harness initial="US" onChangeSpy={spy} />));

    const trigger = host.querySelector<HTMLButtonElement>("#region-select")!;
    act(() => trigger.click());
    const search = document.body.querySelector<HTMLInputElement>('input[role="combobox"]')!;
    act(() => typeInto(search, "cn"));
    act(() => keyDown(search, "Escape"));

    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(spy).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(trigger);
  });

  it("scrolls the selected option into view on open and uses Dialog sheet on narrow viewports", () => {
    const scrollIntoView = vi.fn();
    const origScroll = HTMLElement.prototype.scrollIntoView;
    HTMLElement.prototype.scrollIntoView = scrollIntoView;
    const origMatchMedia = window.matchMedia;
    try {
      act(() => root.render(<Harness initial="SG" />));
      const trigger = host.querySelector<HTMLButtonElement>("#region-select")!;
      act(() => trigger.click());
      expect(scrollIntoView).toHaveBeenCalled();
      act(() => trigger.click());

      // Simulate phone viewport matching SHEET_QUERY
      window.matchMedia = vi.fn((query: string) => ({
        matches: true,
        media: query,
        onchange: null,
        addListener: vi.fn(),
        removeListener: vi.fn(),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        dispatchEvent: vi.fn(),
      })) as unknown as typeof window.matchMedia;

      act(() => root.render(<Harness initial="SG" />));
      const phoneTrigger = host.querySelector<HTMLButtonElement>("#region-select")!;
      act(() => phoneTrigger.click());
      const dialog = document.body.querySelector<HTMLElement>('[role="dialog"]');
      expect(dialog).toBeTruthy();
      expect(dialog?.textContent).toContain("Store Region");
    } finally {
      HTMLElement.prototype.scrollIntoView = origScroll;
      window.matchMedia = origMatchMedia;
    }
  });
});
