import { useEffect, useRef } from "react";
import uPlot from "uplot";
import type { Bucket } from "../types";
import { fillBuckets } from "../query";
import { fmtClock, fmtNum } from "../format";

interface Props {
  buckets: Bucket[];
  /** Called with the dragged window. Omit to make the chart read-only. */
  onSelect?: (from: Date, to: Date) => void;
  onHover?: (text: string) => void;
  theme: string;
  className?: string;
}

/**
 * The histogram, drawn by uPlot.
 *
 * uPlot owns the scale, so a bar can no longer be sized in units its container
 * does not know about — the hand-rolled version computed pixel heights that had
 * to be kept in step with a height in the stylesheet, and when the two drifted
 * the bars painted over the panel header.
 */
export function Histogram({ buckets, onSelect, onHover, theme, className }: Props) {
  const hostRef = useRef<HTMLDivElement>(null);
  const plotRef = useRef<uPlot | null>(null);
  const dataRef = useRef<uPlot.AlignedData>([[], []]);
  // Held in refs so rebuilding the chart on a theme change does not need to
  // re-run whenever a parent re-renders with a new callback identity.
  const selectRef = useRef(onSelect);
  const hoverRef = useRef(onHover);
  selectRef.current = onSelect;
  hoverRef.current = onHover;

  const filled = fillBuckets(buckets ?? []);
  const xs = filled.map((b) => Math.floor(new Date(b.start).getTime() / 1000));
  const ys = filled.map((b) => b.count);
  dataRef.current = [xs, ys];

  // Build (and rebuild on theme change): uPlot resolves colours at construction,
  // so a new palette means a new chart rather than restyling one.
  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;
    const css = getComputedStyle(document.documentElement);
    const bar = css.getPropertyValue("--info-line").trim() || "#888";

    const u = new uPlot(
      {
        width: host.clientWidth || 600,
        height: host.clientHeight || 52,
        padding: [4, 0, 0, 0],
        legend: { show: false },
        cursor: {
          // setScale:false selects without zooming: the range belongs in the
          // query, where it stays visible, editable and shareable.
          drag: { x: true, y: false, setScale: false },
          points: { show: false },
        },
        scales: { x: { time: true }, y: { range: (_u, _min, max) => [0, Math.max(1, max)] } },
        axes: [{ show: false }, { show: false }],
        series: [
          {},
          {
            paths: uPlot.paths.bars!({ size: [0.92, Infinity], align: 1 }),
            fill: bar,
            stroke: bar,
            width: 0,
            points: { show: false },
          },
        ],
        hooks: {
          setSelect: [
            (self) => {
              if (self.select.width <= 2) return;
              const a = self.posToVal(self.select.left, "x");
              const b = self.posToVal(self.select.left + self.select.width, "x");
              self.setSelect({ left: 0, width: 0, top: 0, height: 0 }, false);
              selectRef.current?.(new Date(a * 1000), new Date(b * 1000));
            },
          ],
          setCursor: [
            (self) => {
              const hover = hoverRef.current;
              if (!hover) return;
              const i = self.cursor.idx;
              const [cx, cy] = dataRef.current as [number[], number[]];
              if (i == null || cx[i] === undefined) {
                hover("");
                return;
              }
              hover(`${fmtClock(new Date(cx[i] * 1000).toISOString())} · ${fmtNum(cy[i] ?? 0)} events`);
            },
          ],
        },
      },
      dataRef.current,
      host,
    );
    plotRef.current = u;

    // uPlot needs an explicit pixel size, so it must be told when the container
    // changes — including when a hidden view becomes visible and first gets one.
    const ro = new ResizeObserver(() => {
      if (!host.clientWidth) return;
      u.setSize({ width: host.clientWidth, height: host.clientHeight });
    });
    ro.observe(host);

    return () => {
      ro.disconnect();
      u.destroy();
      plotRef.current = null;
    };
  }, [theme]);

  // Data updates are cheap; they do not rebuild the chart.
  useEffect(() => {
    const u = plotRef.current;
    const host = hostRef.current;
    if (!u || !host) return;
    u.setData(dataRef.current);
    if (host.clientWidth) {
      u.setSize({ width: host.clientWidth, height: host.clientHeight });
    }
  }, [buckets]);

  return <div className={className ?? "chart"} ref={hostRef} />;
}
