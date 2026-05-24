import type { StorybookConfig } from "@storybook/react-vite";
import path from "node:path";

/**
 * Storybook config — stories live alongside the components in
 * `packages/ui/src/components/*.stories.tsx`.  Co-location keeps
 * stories in sync with component refactors (the alternative —
 * a parallel `apps/storybook/stories` tree — accumulates drift
 * as components evolve and stories get forgotten).  We keep
 * apps/storybook around purely as the dev server / build host
 * with its own Storybook-specific deps, but the content
 * authoritatively belongs to @kapp/ui.
 */
const config: StorybookConfig = {
  stories: [
    "../../../packages/ui/src/**/*.stories.@(ts|tsx)",
    "../stories/**/*.stories.@(ts|tsx)",
  ],
  framework: "@storybook/react-vite",
  viteFinal: async (cfg) => {
    // Mirror apps/web's tailwind + path-alias setup so stories
    // resolve @kapp/ui to source (so HMR works on component
    // edits) and Tailwind utilities resolve via the same
    // @tailwindcss/vite plugin pipeline.  The shared design-
    // system globals.css is imported by preview.ts from
    // @kapp/ui/styles/globals.css so design tokens are
    // available in every story without per-story setup.
    const tailwindcss = (await import("@tailwindcss/vite")).default;
    cfg.plugins = cfg.plugins ?? [];
    cfg.plugins.push(tailwindcss());
    cfg.resolve = cfg.resolve ?? {};
    // Vite's resolve.alias is either a `Record<string, string>` or an
    // `Array<{ find, replacement }>` (or one of a few more shapes).
    // Spreading the array form into an object would produce numeric
    // keys (`{ 0: ..., 1: ... }`) and silently drop every existing
    // alias.  Detect the array form and prepend our entry as an
    // array element so we coexist with whatever Storybook / its
    // plugins have already configured.  When it's the object form
    // (Storybook's current default) we still merge by spread, but
    // through a structurally typed view of unknown values so the
    // cast is honest about what we know.
    const aliasKappUi = path.resolve(__dirname, "../../../packages/ui/src");
    const existing = cfg.resolve.alias;
    if (Array.isArray(existing)) {
      cfg.resolve.alias = [
        { find: "@kapp/ui", replacement: aliasKappUi },
        ...existing,
      ];
    } else {
      cfg.resolve.alias = {
        ...(existing as Record<string, unknown> | undefined),
        "@kapp/ui": aliasKappUi,
      };
    }
    return cfg;
  },
};

export default config;
