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
    // @tailwindcss/vite plugin pipeline.  This also imports
    // the same globals.css so design tokens are available in
    // every story without per-story setup.
    const tailwindcss = (await import("@tailwindcss/vite")).default;
    cfg.plugins = cfg.plugins ?? [];
    cfg.plugins.push(tailwindcss());
    cfg.resolve = cfg.resolve ?? {};
    cfg.resolve.alias = {
      ...(cfg.resolve.alias as Record<string, string> | undefined),
      "@kapp/ui": path.resolve(__dirname, "../../../packages/ui/src"),
    };
    return cfg;
  },
};

export default config;
