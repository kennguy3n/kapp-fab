import type { Meta, StoryObj } from "@storybook/react";
import { Avatar, AvatarImage, AvatarFallback, initials } from "./Avatar";

const meta: Meta<typeof Avatar> = {
  title: "UI/Avatar",
  component: Avatar,
  parameters: { layout: "centered" },
  argTypes: {
    size: { control: "select", options: ["xs", "sm", "md", "lg", "xl"] },
  },
};

export default meta;
type Story = StoryObj<typeof Avatar>;

export const WithImage: Story = {
  render: (args) => (
    <Avatar {...args}>
      <AvatarImage
        src="https://images.unsplash.com/photo-1494790108377-be9c29b29330?w=80&h=80&fit=crop"
        alt="Ada Lovelace"
      />
      <AvatarFallback>{initials("Ada Lovelace")}</AvatarFallback>
    </Avatar>
  ),
};

export const FallbackOnly: Story = {
  render: (args) => (
    <Avatar {...args}>
      <AvatarImage src="" alt="Broken URL" />
      <AvatarFallback>{initials("Ken Nguyen")}</AvatarFallback>
    </Avatar>
  ),
};

export const AllSizes: Story = {
  render: () => (
    <div className="flex items-center gap-3">
      <Avatar size="xs">
        <AvatarFallback>XS</AvatarFallback>
      </Avatar>
      <Avatar size="sm">
        <AvatarFallback>SM</AvatarFallback>
      </Avatar>
      <Avatar size="md">
        <AvatarFallback>MD</AvatarFallback>
      </Avatar>
      <Avatar size="lg">
        <AvatarFallback>LG</AvatarFallback>
      </Avatar>
      <Avatar size="xl">
        <AvatarFallback>XL</AvatarFallback>
      </Avatar>
    </div>
  ),
};
