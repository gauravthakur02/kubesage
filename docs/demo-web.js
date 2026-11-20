// Records the web UI demo (assets/demo-web.gif) with Playwright and ffmpeg. Use a throwaway kind cluster set up as in
// docs/demo.tape, so no real cluster or namespace names end up in the GIF:
//
//   KUBECONFIG=<demo kubeconfig> HOME=$DEMO_HOME kubeweave -n shop serve --no-browser --addr 127.0.0.1:7788 > serve.log &
//   npm install playwright-core@1.62.1        # in a scratch folder; uses your installed Google Chrome
//   node docs/demo-web.js serve.log video/
//   ffmpeg -i video/*.webm -vf "fps=10,scale=960:-1:flags=lanczos,palettegen=max_colors=128:stats_mode=diff" pal.png
//   ffmpeg -i video/*.webm -i pal.png -lavfi "fps=10,scale=960:-1:flags=lanczos[x];[x][1:v]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle" assets/demo-web.gif
const { chromium } = require('playwright-core');
const fs = require('fs');
const url = fs.readFileSync(process.argv[2], 'utf8').match(/http:\/\/127\.0\.0\.1:\d+\/\?t=\w+/)[0];
const out = process.argv[3];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

(async () => {
  const browser = await chromium.launch({ channel: 'chrome', headless: true });
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 720 }, colorScheme: 'dark',
    recordVideo: { dir: out, size: { width: 1280, height: 720 } } });
  // a visible mouse pointer (headless video has none)
  await ctx.addInitScript(() => {
    addEventListener('DOMContentLoaded', () => {
      const d = document.createElement('div');
      Object.assign(d.style, { position: 'fixed', left: '0', top: '0', width: '18px', height: '18px', borderRadius: '50%', zIndex: 99999,
        background: 'rgba(255,255,255,.85)', border: '2px solid #1a1b26', pointerEvents: 'none', transform: 'translate(-50%,-50%)', transition: 'width .1s,height .1s' });
      document.body.appendChild(d);
      addEventListener('mousemove', (e) => { d.style.left = e.clientX + 'px'; d.style.top = e.clientY + 'px'; }, true);
      addEventListener('mousedown', () => { d.style.width = d.style.height = '12px'; }, true);
      addEventListener('mouseup', () => { d.style.width = d.style.height = '18px'; }, true);
    });
  });
  const page = await ctx.newPage();
  const click = async (loc, dbl) => {
    const b = await loc.boundingBox();
    await page.mouse.move(b.x + Math.min(b.width / 2, 60), b.y + b.height / 2, { steps: 18 });
    await sleep(150);
    await (dbl ? loc.dblclick() : loc.click());
  };
  const t0 = Date.now();
  const step = (n) => console.log(((Date.now() - t0) / 1000).toFixed(1), n);
  try {
  await page.goto(url);
  await page.locator('#table tbody tr').nth(5).waitFor();
  await page.mouse.move(640, 400);
  await sleep(1800);

  step('table');
  // the crashing pod and its logs
  await click(page.locator('#table tbody tr', { hasText: 'worker' }).first(), true);
  await page.locator('#tab-logs').waitFor();
  await sleep(500);
  await click(page.locator('#tab-logs'));
  await page.getByText('FATAL').first().waitFor({ timeout: 8000 }).catch(() => {});
  await sleep(2600);

  step('logs');
  // a deployment's relationships
  await click(page.locator('#side a[title="deployments"]').first());
  await page.locator('#table tbody tr', { hasText: 'web' }).first().waitFor();
  await sleep(600);
  await click(page.locator('#table tbody tr', { hasText: 'web' }).first(), true);
  await page.locator('#tab-graph').waitFor();
  await sleep(300);
  await click(page.locator('#tab-graph'));
  await sleep(2800);

  step('graph');
  // go anywhere with Ctrl+K: the health check
  await page.keyboard.press('Control+k');
  await sleep(500);
  await page.keyboard.type('health', { delay: 110 });
  await sleep(500);
  await page.keyboard.press('Enter');
  await sleep(4200);
  step('done');
  } catch (e) { console.log('FAILED', e.message.split('\n')[0]); await page.screenshot({ path: 'fail.png' }); }
  await ctx.close();
  await browser.close();
})();
