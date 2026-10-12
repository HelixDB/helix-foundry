import { expect, test } from "@playwright/test";
import { api, openApp } from "./support/workspace";
import { datasetTable } from "../../packages/shared/src/index.js";

test("native Analyst profit answers show signed bars around zero on desktop and mobile", async ({
  page,
}, testInfo) => {
  await openApp(page);
  const ws = await api(page, "/workspaces", {
    name: "Signed charts " + Date.now(),
  });
  const base = `/workspaces/${ws.id}`;
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    ws.id,
  );
  await api(page, base + "/onboarding/provider", {
    provider: "local",
    model: "qwen3:4b",
  });
  const cases: [string, number[]][] = [
    ["mixed", [-50, 100, 0]],
    ["negative", [-80, -20]],
    ["positive", [100, 25]],
    ["zero", [0, 0]],
  ];
  let published = false;
  for (const [name, values] of cases) {
    const goal = "Signed breakdown " + name;
    const ds = await api(page, base + "/ingest", {
      name: goal,
      rows: values.map((profit, i) => ({ category: `Category ${i}`, profit })),
    });
    if (!published) {
      const proposal = (
        await api(page, base + "/definitions/import", {
          version: 1,
          proposals: [
            {
              summary: "Profit analysis",
              assumptions: [],
              catalog: [],
              pipelines: [],
              ontology: [],
              relationships: [],
              dashboards: [
                {
                  name: "Profit",
                  widgets: [
                    {
                      title: "Profit",
                      type: "metric",
                      inputs: [ds.id],
                      sql: `SELECT sum(profit) profit FROM ${datasetTable(ds.id)}`,
                    },
                  ],
                },
              ],
              actions: [],
            },
          ],
        })
      ).items[0];
      expect(proposal.data.validation.valid).toBe(true);
      await api(page, base + `/proposals/${proposal.id}/publish`, {
        hash: proposal.data.hash,
      });
      published = true;
    }
    const run = await api(page, base + "/assistant/runs", {
      goal,
      intent: "answer",
    });
    let done: any;
    await expect
      .poll(
        async () => {
          done = await api(page, base + `/resources/run/${run.id}`);
          return done.data.status;
        },
        { timeout: 30000 },
      )
      .toBe("succeeded");
    expect(done.data.answer.rows.map((r: any) => Number(r.profit))).toEqual(
      values,
    );
    expect(done.data.answer.citations).toContainEqual(
      expect.objectContaining({
        datasetId: ds.id,
        version: ds.data.activeVersion,
      }),
    );
    await page.goto("/analyst/" + run.id);
    const chart = page.locator(".an-chart");
    await expect(chart).toBeVisible();
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: 844 });
      await page.emulateMedia({ reducedMotion: "reduce" });
      const bars = chart.locator(".wk-bar");
      await expect(bars).toHaveCount(values.length);
      const geometry = await bars.evaluateAll((els) =>
        els.map((el) => {
          const bar = el.getBoundingClientRect(),
            track = el.parentElement!.getBoundingClientRect();
          return {
            width: bar.width / track.width,
            left: (bar.left - track.left) / track.width,
          };
        }),
      );
      const lower = Math.min(0, ...values),
        upper = Math.max(0, ...values),
        range = upper - lower || 1;
      for (let i = 0; i < values.length; i++) {
        expect(geometry[i].width).toBeCloseTo(Math.abs(values[i]) / range, 2);
        expect(geometry[i].left).toBeCloseTo(
          (Math.min(0, values[i]) - lower) / range,
          2,
        );
      }
      if (values.some((v) => v < 0)) {
        const zeroLines = chart.locator(".wk-bar-zero");
        await expect(zeroLines).toHaveCount(values.length);
        const visible = await zeroLines.evaluateAll((els) =>
          els.map((el) => {
            const style = getComputedStyle(el);
            return {
              width: style.borderLeftWidth,
              style: style.borderLeftStyle,
              color: style.borderLeftColor,
            };
          }),
        );
        for (const line of visible) {
          expect(line.width).toBe("1px");
          expect(line.style).toBe("solid");
          expect(line.color).not.toBe("rgba(0, 0, 0, 0)");
        }
      }
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= innerWidth,
        ),
      ).toBe(true);
    }
    await page.screenshot({
      path: testInfo.outputPath(name + "-mobile.png"),
      fullPage: true,
    });
    const image = chart.getByRole("img");
    await image.focus();
    await expect(chart.locator('[aria-live="polite"]')).toHaveText(
      `Category 0: ${values[0]}`,
    );
    await page.keyboard.press("ArrowDown");
    await expect(chart.locator('[aria-live="polite"]')).toHaveText(
      `Category 1: ${values[1]}`,
    );
  }
});
