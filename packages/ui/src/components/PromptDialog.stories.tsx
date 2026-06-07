import type { Meta, StoryObj } from "@storybook/react";
import { useState } from "react";
import { PromptDialog } from "./PromptDialog";
import { Button } from "./Button";

const meta: Meta = {
  title: "UI/PromptDialog",
};

export default meta;
type Story = StoryObj;

export const Default: Story = {
  render: () => {
    const [open, setOpen] = useState(false);
    const [saved, setSaved] = useState<string | null>(null);
    return (
      <div className="space-y-2">
        <Button onClick={() => setOpen(true)}>Save view</Button>
        {saved && <p className="text-sm text-fg-muted">Saved as: {saved}</p>}
        <PromptDialog
          open={open}
          onOpenChange={setOpen}
          title="Save current view"
          description="Give this filtered view a name to reuse later."
          label="View name"
          placeholder="e.g. Overdue invoices"
          onSubmit={(value) => {
            setSaved(value);
            setOpen(false);
          }}
        />
      </div>
    );
  },
};
