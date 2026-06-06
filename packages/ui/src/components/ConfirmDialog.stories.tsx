import type { Meta, StoryObj } from "@storybook/react";
import { useState } from "react";
import { ConfirmDialog } from "./ConfirmDialog";
import { Button } from "./Button";

const meta: Meta = {
  title: "UI/ConfirmDialog",
};

export default meta;
type Story = StoryObj;

export const Default: Story = {
  render: () => {
    const [open, setOpen] = useState(false);
    return (
      <div>
        <Button onClick={() => setOpen(true)}>Publish</Button>
        <ConfirmDialog
          open={open}
          onOpenChange={setOpen}
          title="Publish this report?"
          description="Recipients will be notified by email immediately."
          confirmLabel="Publish"
          onConfirm={() => setOpen(false)}
        />
      </div>
    );
  },
};

export const Destructive: Story = {
  render: () => {
    const [open, setOpen] = useState(false);
    return (
      <div>
        <Button variant="destructive" onClick={() => setOpen(true)}>
          Delete view
        </Button>
        <ConfirmDialog
          open={open}
          onOpenChange={setOpen}
          title="Delete this view?"
          description="This action cannot be undone."
          confirmLabel="Delete"
          destructive
          onConfirm={() => setOpen(false)}
        />
      </div>
    );
  },
};
