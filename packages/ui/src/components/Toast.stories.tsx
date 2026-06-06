import type { Meta, StoryObj } from "@storybook/react";
import { Toaster, toast } from "./Toast";
import { Button } from "./Button";

const meta: Meta = {
  title: "UI/Toast",
};

export default meta;
type Story = StoryObj;

export const Playground: Story = {
  render: () => (
    <div className="flex flex-wrap gap-2">
      <Button onClick={() => toast.success("Record created successfully")}>
        Success
      </Button>
      <Button
        variant="destructive"
        onClick={() =>
          toast.error("Export failed", {
            description: "The server returned a 500 error.",
          })
        }
      >
        Error
      </Button>
      <Button
        variant="outline"
        onClick={() => toast.warning("Unsaved changes will be lost")}
      >
        Warning
      </Button>
      <Button variant="outline" onClick={() => toast.info("Sync in progress")}>
        Info
      </Button>
      <Toaster />
    </div>
  ),
};
