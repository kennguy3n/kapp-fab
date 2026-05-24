import type { Meta, StoryObj } from "@storybook/react";
import { useState } from "react";
import {
  Modal,
  ModalTrigger,
  ModalContent,
  ModalHeader,
  ModalTitle,
  ModalDescription,
  ModalFooter,
  ModalClose,
  ControlledModal,
} from "./Modal";
import { Button } from "./Button";
import { Input } from "./Input";

const meta: Meta = {
  title: "UI/Modal",
};

export default meta;
type Story = StoryObj;

export const Composable: Story = {
  render: () => (
    <Modal>
      <ModalTrigger asChild>
        <Button>Open dialog</Button>
      </ModalTrigger>
      <ModalContent>
        <ModalHeader>
          <ModalTitle>Edit profile</ModalTitle>
          <ModalDescription>
            Update your display name. Changes are saved immediately.
          </ModalDescription>
        </ModalHeader>
        <div className="flex flex-col gap-2">
          <label className="text-sm font-medium" htmlFor="name">
            Display name
          </label>
          <Input id="name" defaultValue="Ken Nguyen" />
        </div>
        <ModalFooter>
          <ModalClose asChild>
            <Button variant="outline">Cancel</Button>
          </ModalClose>
          <ModalClose asChild>
            <Button>Save</Button>
          </ModalClose>
        </ModalFooter>
      </ModalContent>
    </Modal>
  ),
};

export const Controlled: Story = {
  render: () => {
    const [open, setOpen] = useState(false);
    return (
      <div>
        <Button onClick={() => setOpen(true)}>Open controlled modal</Button>
        <ControlledModal
          open={open}
          onClose={() => setOpen(false)}
          title="Controlled dialog"
        >
          <p className="text-sm text-fg-muted">
            This uses the legacy <code>open</code> / <code>onClose</code> API.
          </p>
          <ModalFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>
              Close
            </Button>
          </ModalFooter>
        </ControlledModal>
      </div>
    );
  },
};
