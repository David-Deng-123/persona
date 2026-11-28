// Reusable searchable single-select control for long option lists (e.g., > 8 extension enum/oneOf choices).
// Desktop opens a viewport-safe portal popover anchored to the trigger; phone widths fall back to Dialog's bottom sheet.
import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { createPortal } from "react-dom";
import { t } from "../i18n";
import { composing } from "../lib/send-history";
import { cx, Dialog, IconButton } from "./atoms";
import { Icon } from "./icons";
import { placeMenu, SHEET_QUERY, useEscapeClose, useMedia, type MenuPlace } from "./overlay";
import { SEARCH_FOCUS } from "./SearchField";

/** Number of options above which a native <select> upgrades to SearchableSelect. */
export const SEARCHABLE_SELECT_THRESHOLD = 8;

export interface SearchableSelectOption<T = string> {
  value: T;
  /** Localized or human-readable display label. */
  label: string;
  /** Raw option value/code string used for code matching and optional secondary hint. */
  rawValue?: string;
}

/** Match rank tiers (lower is better): exact value > exact label > value/label prefix > substring/multi-token. */
export const enum SearchMatchRank {
  ExactValue = 0,
  ExactLabel = 1,
  Prefix = 2,
  Substring = 3,
}

export function optionRawValue<T>(option: SearchableSelectOption<T>): string {
  if (option.rawValue !== undefined) return option.rawValue;
  if (option.value === null || option.value === undefined) return "";
  return typeof option.value === "string" ? option.value : typeof option.value === "object" ? JSON.stringify(option.value) : String(option.value);
}

/** Splits a search query into lowercased non-empty whitespace tokens. */
export function tokenizeSearchQuery(query: string): string[] {
  const trimmed = query.trim().toLowerCase();
  return trimmed ? trimmed.split(/\s+/) : [];
}

/**
 * Ranks a single option against a query (matching both display label and raw value/code, case-insensitively,
 * with AND semantics across whitespace-separated tokens). Returns null when the option does not match.
 */
export function rankSearchableOption<T>(option: SearchableSelectOption<T>, query: string): SearchMatchRank | null {
  const tokens = tokenizeSearchQuery(query);
  if (!tokens.length) return SearchMatchRank.Substring;

  const labelNorm = option.label.trim().toLowerCase();
  const valueNorm = optionRawValue(option).trim().toLowerCase();

  if (!tokens.every((tok) => valueNorm.includes(tok) || labelNorm.includes(tok))) return null;

  const normalized = tokens.join(" ");
  if (valueNorm !== "" && valueNorm === normalized) return SearchMatchRank.ExactValue;
  if (labelNorm !== "" && labelNorm === normalized) return SearchMatchRank.ExactLabel;
  if ((valueNorm !== "" && valueNorm.startsWith(normalized)) || (labelNorm !== "" && labelNorm.startsWith(normalized))) {
    return SearchMatchRank.Prefix;
  }
  return SearchMatchRank.Substring;
}

/**
 * Filters and ranks options for `query`. Preserves original declaration order as a deterministic tie-breaker
 * within the same rank tier (and returns all options unchanged when the query is blank).
 */
export function filterSearchableOptions<T>(options: readonly SearchableSelectOption<T>[], query: string): SearchableSelectOption<T>[] {
  if (!query.trim()) return [...options];
  const scored: { option: SearchableSelectOption<T>; rank: SearchMatchRank; index: number }[] = [];
  for (let i = 0; i < options.length; i++) {
    const rank = rankSearchableOption(options[i], query);
    if (rank !== null) scored.push({ option: options[i], rank, index: i });
  }
  scored.sort((a, b) => a.rank - b.rank || a.index - b.index);
  return scored.map((entry) => entry.option);
}

function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/**
 * Returns a subtle secondary code hint when `rawValue` is distinct from `label` and not already visible
 * as a standalone token inside `label`.
 */
export function optionSecondaryHint(label: string, rawValue: string): string | null {
  const raw = rawValue.trim();
  const lbl = label.trim();
  if (!raw || !lbl) return null;
  if (lbl.toLowerCase() === raw.toLowerCase()) return null;
  // ASCII codes check token boundaries so e.g. "us" does not falsely match inside "Australia".
  if (/^[a-z0-9][a-z0-9_-]*$/i.test(raw)) {
    const tokenPattern = new RegExp(`(?<![a-z0-9])${escapeRegExp(raw)}(?![a-z0-9])`, "i");
    if (tokenPattern.test(lbl)) return null;
  } else if (lbl.toLowerCase().includes(raw.toLowerCase())) {
    return null;
  }
  return raw;
}

/** Visible row inside the listbox: either the clear/default placeholder or a concrete option. */
type PickerRow<T> =
  | { kind: "clear"; key: string; label: string }
  | { kind: "option"; key: string; option: SearchableSelectOption<T>; hint: string | null };

const DEFAULT_EQUALITY = <T,>(a: T, b: T) => Object.is(a, b);
const POPOVER_EST_HEIGHT = 360;
const POPOVER_MIN_WIDTH = 280;

export interface SearchableSelectProps<T = string> {
  id: string;
  /** Accessible title for the listbox and mobile sheet header. */
  title: string;
  value: T | undefined;
  options: readonly SearchableSelectOption<T>[];
  /** Empty/default option label shown when value is undefined and selectable to clear. */
  placeholder: string;
  onChange: (value: T | undefined) => void;
  /** Custom equality comparator for non-primitive JSON values. */
  equals?: (a: T, b: T) => boolean;
  /** Search input placeholder; defaults to localized "Search options…". */
  searchPlaceholder?: string;
  /** Empty-state text when no options match; defaults to localized "No matching options". */
  emptyText?: string;
  disabled?: boolean;
  invalid?: boolean;
  className?: string;
}

export function SearchableSelect<T = string>({
  id,
  title,
  value,
  options,
  placeholder,
  onChange,
  equals = DEFAULT_EQUALITY,
  searchPlaceholder,
  emptyText,
  disabled = false,
  invalid = false,
  className,
}: SearchableSelectProps<T>) {
  const listboxId = useId();
  const sheet = useMedia(SHEET_QUERY);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const searchInputRef = useRef<HTMLInputElement>(null);
  const rowRefs = useRef<(HTMLButtonElement | null)[]>([]);

  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [activeIndex, setActiveIndex] = useState(0);
  const [place, setPlace] = useState<MenuPlace | null>(null);

  const selectedIndex = useMemo(
    () => (value === undefined ? -1 : options.findIndex((o) => equals(o.value, value))),
    [options, value, equals],
  );
  const selectedOption = selectedIndex >= 0 ? options[selectedIndex] : undefined;

  const trimmedQuery = query.trim();
  const filteredOptions = useMemo(() => filterSearchableOptions(options, query), [options, query]);

  const rows = useMemo<PickerRow<T>[]>(() => {
    const out: PickerRow<T>[] = [];
    if (!trimmedQuery) {
      out.push({ kind: "clear", key: "__clear__", label: placeholder });
    }
    for (let i = 0; i < filteredOptions.length; i++) {
      const option = filteredOptions[i];
      const raw = optionRawValue(option);
      out.push({
        kind: "option",
        key: `${raw}:${i}`,
        option,
        hint: optionSecondaryHint(option.label, raw),
      });
    }
    return out;
  }, [trimmedQuery, placeholder, filteredOptions]);

  const isRowSelected = useCallback(
    (row: PickerRow<T>) => {
      if (row.kind === "clear") return selectedOption === undefined;
      return selectedOption !== undefined && equals(row.option.value, selectedOption.value);
    },
    [selectedOption, equals],
  );

  const close = useCallback((restoreFocus: boolean) => {
    setOpen(false);
    setQuery("");
    if (restoreFocus) {
      // Defer when closing a mobile Dialog so Dialog's modal focus cleanup and our focus call stay consistent.
      requestAnimationFrame(() => triggerRef.current?.focus({ preventScroll: true }));
      triggerRef.current?.focus({ preventScroll: true });
    }
  }, []);

  const openPicker = useCallback(() => {
    if (disabled) return;
    setQuery("");
    // Position the initial active highlight on the currently selected row (clear row at 0 when unset).
    setActiveIndex(selectedIndex >= 0 ? selectedIndex + 1 : 0);
    setOpen(true);
  }, [disabled, selectedIndex]);

  const selectRow = useCallback(
    (row: PickerRow<T>) => {
      onChange(row.kind === "clear" ? undefined : row.option.value);
      close(true);
    },
    [onChange, close],
  );

  // Reset active highlight to the top result whenever the search query changes.
  useEffect(() => {
    if (!open) return;
    if (trimmedQuery) {
      setActiveIndex(rows.length > 0 ? 0 : -1);
    } else {
      setActiveIndex(selectedIndex >= 0 ? selectedIndex + 1 : 0);
    }
  }, [open, trimmedQuery, rows.length, selectedIndex]);

  // Keep desktop popover anchored safely inside the viewport, never clipped by Settings scroll containers.
  const measurePlace = useCallback(() => {
    const anchor = triggerRef.current;
    if (!anchor) return;
    const rect = anchor.getBoundingClientRect();
    const panelHeight = panelRef.current?.offsetHeight || POPOVER_EST_HEIGHT;
    const width = Math.max(rect.width, POPOVER_MIN_WIDTH);
    setPlace(
      placeMenu(rect, panelHeight, {
        width,
        align: "left",
        side: "bottom",
        screenW: window.innerWidth,
        screenH: window.innerHeight,
        beside: true,
      }),
    );
  }, []);

  useLayoutEffect(() => {
    if (!open || sheet) return;
    measurePlace();
    const onScroll = (e: Event) => {
      if (panelRef.current?.contains(e.target as Node)) return;
      measurePlace();
    };
    window.addEventListener("resize", measurePlace);
    window.addEventListener("scroll", onScroll, true);
    return () => {
      window.removeEventListener("resize", measurePlace);
      window.removeEventListener("scroll", onScroll, true);
    };
  }, [open, sheet, measurePlace, rows.length]);

  // Auto-focus search input on desktop open and scroll the selected item into view.
  useEffect(() => {
    if (!open) return;
    if (!sheet) searchInputRef.current?.focus({ preventScroll: true });
    const targetIdx = selectedIndex >= 0 ? selectedIndex + 1 : 0;
    const el = rowRefs.current[targetIdx];
    if (el && typeof el.scrollIntoView === "function") el.scrollIntoView({ block: "nearest" });
  }, [open, sheet, selectedIndex]);

  // Scroll newly highlighted row into view during keyboard navigation.
  useEffect(() => {
    if (!open || activeIndex < 0) return;
    const el = rowRefs.current[activeIndex];
    if (el && typeof el.scrollIntoView === "function") el.scrollIntoView({ block: "nearest" });
  }, [open, activeIndex]);

  // Dismiss desktop popover on outside pointer interaction.
  useEffect(() => {
    if (!open || sheet) return;
    const onPointerDown = (e: PointerEvent) => {
      const target = e.target as Node | null;
      if (panelRef.current?.contains(target) || triggerRef.current?.contains(target)) return;
      close(false);
    };
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [open, sheet, close]);

  useEscapeClose(open && !sheet, () => close(true));

  const optionDomId = (index: number) => `${listboxId}-opt-${index}`;
  const activeDomId = open && activeIndex >= 0 && activeIndex < rows.length ? optionDomId(activeIndex) : undefined;

  const handleKeyDown = (e: KeyboardEvent<HTMLElement>) => {
    if (composing(e.nativeEvent)) return;
    if (e.key === "ArrowDown") {
      e.preventDefault();
      if (!rows.length) return;
      setActiveIndex((cur) => (cur < 0 ? 0 : (cur + 1) % rows.length));
      return;
    }
    if (e.key === "ArrowUp") {
      e.preventDefault();
      if (!rows.length) return;
      setActiveIndex((cur) => (cur <= 0 ? rows.length - 1 : cur - 1));
      return;
    }
    if (e.key === "Enter") {
      if (activeIndex >= 0 && activeIndex < rows.length) {
        e.preventDefault();
        selectRow(rows[activeIndex]);
      }
      return;
    }
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      close(true);
      return;
    }
    if (e.key === "Tab" && !sheet) {
      close(false);
    }
  };

  const handleTriggerKeyDown = (e: KeyboardEvent<HTMLButtonElement>) => {
    if (disabled) return;
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      openPicker();
    }
  };

  const resolvedSearchPlaceholder = searchPlaceholder ?? t("ui-searchableselect.search_placeholder");
  const resolvedEmptyText = emptyText ?? t("ui-searchableselect.no_match");

  rowRefs.current.length = rows.length;

  const pickerBody = (
    <div className="flex min-h-0 flex-1 flex-col gap-2">
      <label className={cx("flex h-8 shrink-0 items-center gap-2 rounded-control bg-inset px-2.5 shadow-hairline", SEARCH_FOCUS)}>
        <Icon name="search" size={13} stroke={2} className="shrink-0 text-ink-3" />
        <input
          ref={searchInputRef}
          role="combobox"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={handleKeyDown}
          autoFocus={!sheet}
          placeholder={resolvedSearchPlaceholder}
          aria-label={resolvedSearchPlaceholder}
          aria-autocomplete="list"
          aria-controls={listboxId}
          aria-activedescendant={activeDomId}
          aria-expanded={open}
          className="min-w-0 flex-1 bg-transparent text-tree-row text-ink outline-none placeholder:text-ink-3"
        />
        {query && (
          <IconButton
            icon="x"
            label={t("ui-searchfield.clear")}
            size={20}
            iconSize={11}
            onClick={() => {
              setQuery("");
              if (!sheet) searchInputRef.current?.focus({ preventScroll: true });
            }}
          />
        )}
      </label>
      <div
        id={listboxId}
        role="listbox"
        aria-label={title}
        onKeyDown={handleKeyDown}
        className="flex min-h-0 max-h-[50vh] flex-1 flex-col gap-0.5 overflow-y-auto overscroll-contain"
      >
        {rows.map((row, i) => {
          const selected = isRowSelected(row);
          const active = i === activeIndex;
          const isClear = row.kind === "clear";
          return (
            <button
              key={row.key}
              id={optionDomId(i)}
              ref={(el) => {
                rowRefs.current[i] = el;
              }}
              type="button"
              role="option"
              aria-selected={selected}
              data-active={active || undefined}
              data-clear-option={isClear || undefined}
              onMouseEnter={() => setActiveIndex(i)}
              onClick={() => selectRow(row)}
              className={cx(
                "flex min-h-9 w-full shrink-0 items-center gap-2 rounded-control px-2.5 py-1.5 text-left text-meta whitespace-nowrap transition-colors pointer-coarse:min-h-11",
                selected
                  ? active
                    ? "bg-accent-tint font-medium text-accent-ink shadow-[inset_0_0_0_1px_var(--accent)]"
                    : "bg-accent-tint font-medium text-accent-ink"
                  : active
                    ? "bg-hover text-ink"
                    : "text-ink hover:bg-hover",
                isClear && !selected && "text-ink-2",
              )}
            >
              <span className="min-w-0 flex-1 truncate">{row.kind === "clear" ? row.label : row.option.label}</span>
              {row.kind === "option" && row.hint && (
                <span className={cx("shrink-0 font-mono text-submeta", selected ? "text-accent-ink" : "text-ink-3")}>
                  {row.hint}
                </span>
              )}
              <Icon
                name="check"
                size={13}
                stroke={2.5}
                className={cx("shrink-0", selected ? "text-accent-ink" : "invisible")}
              />
            </button>
          );
        })}
        {!rows.length && (
          <p role="status" className="px-2.5 py-4 text-center text-label text-ink-3">
            {resolvedEmptyText}
          </p>
        )}
      </div>
    </div>
  );

  return (
    <>
      <button
        ref={triggerRef}
        id={id}
        type="button"
        disabled={disabled}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={open ? listboxId : undefined}
        aria-invalid={invalid || undefined}
        onClick={() => (open ? close(true) : openPicker())}
        onKeyDown={handleTriggerKeyDown}
        className={cx(
          "flex h-8 w-full items-center gap-2 rounded-control bg-field px-2.5 text-left text-tree-row whitespace-nowrap text-ink outline-none focus:shadow-[inset_0_0_0_1px_var(--accent)] disabled:opacity-60",
          invalid ? "shadow-[inset_0_0_0_1px_var(--red)]" : "shadow-inset-field",
          className,
        )}
      >
        <span className={cx("min-w-0 flex-1 truncate", !selectedOption && "text-ink-3")}>
          {selectedOption ? selectedOption.label : placeholder}
        </span>
        <Icon
          name="chevron"
          size={12}
          className={cx("shrink-0 text-ink-3 transition-transform duration-150", open && "rotate-180")}
        />
      </button>

      {open && sheet && (
        <Dialog open onClose={() => close(true)} width={380} title={title}>
          {pickerBody}
        </Dialog>
      )}

      {open && !sheet && createPortal(
        <div
          ref={panelRef}
          data-searchable-select-popover=""
          className="fixed z-50 flex flex-col overflow-hidden rounded-card bg-surface p-2 shadow-overlay"
          style={{
            left: place?.left ?? 8,
            top: place?.top ?? 8,
            width: place?.width ?? POPOVER_MIN_WIDTH,
            maxHeight: place?.maxHeight,
            animation: "pop-in 160ms var(--ease-spring) both",
          }}
        >
          {pickerBody}
        </div>,
        document.body,
      )}
    </>
  );
}
