import type { Preview } from "@storybook/react";
import "../../web/src/styles/globals.css";

/**
 * Storybook preview config — imports apps/web's globals.css so
 * design tokens, Tailwind utilities, and tw-animate-css are
 * available in every story.  Importing from apps/web rather
 * than duplicating the CSS guarantees stories show the exact
 * tokens the production app uses; a parallel copy would drift.
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
  globalTypes: {
    theme: {
      description: "Light / dark colour scheme",
      defaultValue: "light",
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
