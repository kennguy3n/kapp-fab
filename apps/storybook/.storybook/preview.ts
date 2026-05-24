import type { Preview } from "@storybook/react";
import "@kapp/ui/styles/globals.css";

/**
 * Storybook preview config — imports the @kapp/ui design system's
 * globals.css so design tokens, Tailwind utilities, and
 * tw-animate-css are available in every story.  The stylesheet
 * lives inside packages/ui (the package that owns the design
 * system) rather than apps/web; Storybook and apps/web both pull
 * it via the @kapp/ui exports map, so there's a single source of
 * truth and the previous ../../web/src/styles cross-package path
 * — which would have silently broken Storybook builds if the
 * stylesheet moved — is gone.
 *
 * The light/dark scheme switcher lives in the toolbar via the
 * Storybook globalTypes mechanism so designers can preview both
 * modes per story.  The decorator below toggles the `.dark`
 * class on the document root which matches how the production
 * app's theme controller switches schemes.
 */
const preview: Preview = {
  parameters: {
    controls: {
      matchers: {
        color: /(background|color)$/i,
        date: /Date$/i,
      },
    },
    backgrounds: {
      default: "app",
      values: [
        { name: "app", value: "var(--bg)" },
        { name: "subtle", value: "var(--bg-subtle)" },
        { name: "elevated", value: "var(--bg-elevated)" },
      ],
    },
  },
  // Storybook 8 removed `globalTypes.<name>.defaultValue`; the
  // replacement is the top-level `initialGlobals` map.  See
  // https://storybook.js.org/docs/api/main-config-globals  We
  // still rely on the decorator's `?? "light"` fallback as a
  // belt-and-braces guard in case Storybook ever loads a global
  // map with a missing key (e.g. on first run after upgrading).
  initialGlobals: {
    theme: "light",
  },
  globalTypes: {
    theme: {
      description: "Light / dark colour scheme",
      toolbar: {
        title: "Theme",
        icon: "circlehollow",
        items: [
          { value: "light", title: "Light", icon: "sun" },
          { value: "dark", title: "Dark", icon: "moon" },
        ],
        dynamicTitle: true,
      },
    },
  },
  decorators: [
    (Story, ctx) => {
      const theme = (ctx.globals.theme as string) ?? "light";
      if (typeof document !== "undefined") {
        document.documentElement.classList.toggle("dark", theme === "dark");
      }
      return Story();
    },
  ],
};

export default preview;
